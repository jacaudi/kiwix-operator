//go:build e2e

// Package e2e runs the operator end to end in the throwaway kind cluster that `task e2e` creates.
package e2e

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/jacaudi/kiwix-operator/api/v1alpha1"
)

const (
	// kindContext is the only kubeconfig context these tests may touch.
	kindContext = "kind-kiwix-e2e"
	ns          = "kiwix-e2e"
	toolsNS     = "kiwix-e2e-tools"
	operatorNS  = "kiwix-system"
	release     = "kiwix-operator"
	libraryName = "library"
	siteName    = "site"
	bookName    = "book"
	// catalogBook is any name: the fake catalog answers every query with book-01.zim.
	catalogBook = "e2e_book"
	// catalogVersion is the month of the fake catalog entry's <updated>.
	catalogVersion = "2026-10"
	siteURL        = "http://site.kiwix-e2e.svc.cluster.local/"
	webhookURL     = "http://sink.kiwix-e2e-tools.svc:8080/hooks/kiwix"
	poll           = 5 * time.Second
)

var (
	repoRoot string
	restCfg  *rest.Config
)

func TestMain(m *testing.M) {
	if err := setup(); err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// setup refuses to run unless the loaded kubeconfig's current context is the kind cluster, then resolves the repo root.
func setup() error {
	raw, err := clientcmd.NewDefaultClientConfigLoadingRules().Load()
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	if raw.CurrentContext != kindContext {
		return fmt.Errorf("refusing to run: the kubeconfig's current context is %q, want %q (run through task e2e)", raw.CurrentContext, kindContext)
	}
	restCfg, err = clientcmd.NewDefaultClientConfig(*raw, &clientcmd.ConfigOverrides{CurrentContext: kindContext}).ClientConfig()
	if err != nil {
		return fmt.Errorf("build client config: %w", err)
	}
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return fmt.Errorf("resolve the repo root: %w", err)
	}
	repoRoot = strings.TrimSpace(string(out))
	return nil
}

type env struct {
	c  client.Client
	cs kubernetes.Interface
	// dumped records that a subtest already logged the diagnostics.
	dumped bool
}

func TestE2E(t *testing.T) {
	e := newEnv(t)
	t.Cleanup(func() {
		if t.Failed() && !e.dumped {
			e.dump(t)
		}
	})
	e.setupLibrary(t)

	crawled := t.Run("crawlServedAndNotified", e.crawlServedAndNotified)
	t.Run("catalogDownload", e.catalogDownload)
	// The rest act on the crawled archive, each on the state the one before it left.
	if !crawled || !t.Run("refreshRejected", e.refreshRejected) || !t.Run("deleteRetainKeepsFile", e.deleteRetainKeepsFile) {
		t.Log("skipping the subtests that depend on an earlier failed one")
		return
	}
	t.Run("crawlEgressBlockedWithoutFlag", e.crawlEgressBlockedWithoutFlag)
}

func newEnv(t *testing.T) *env {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	c, err := client.New(restCfg, client.Options{Scheme: scheme})
	require.NoError(t, err)
	cs, err := kubernetes.NewForConfig(restCfg)
	require.NoError(t, err)
	return &env{c: c, cs: cs}
}

// setupLibrary starts the site, waits for the operator and the tools, then creates the KiwixLibrary and waits until it serves.
func (e *env) setupLibrary(t *testing.T) {
	ctx := t.Context()
	_, err := kubectl(ctx, "apply", "-f", filepath.Join(repoRoot, "test/e2e/manifests/site.yaml"))
	require.NoError(t, err)
	for _, namespace := range []string{operatorNS, toolsNS, ns} {
		_, err := kubectl(ctx, "wait", "--for=condition=Available", "deployment", "--all", "-n", namespace, "--timeout=5m")
		require.NoError(t, err)
	}

	lib := &v1alpha1.KiwixLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: libraryName, Namespace: ns},
		Spec: v1alpha1.KiwixLibrarySpec{
			Storage: v1alpha1.StorageSpec{Size: new(resource.MustParse("1Gi"))},
			// The kind volume shares the docker host's disk, so the default 5Gi reserve could refuse every write.
			ReserveSpace: resource.MustParse("1Mi"),
			Notify:       &v1alpha1.NotifySpec{Webhook: v1alpha1.WebhookSpec{URL: webhookURL}},
		},
	}
	require.NoError(t, e.c.Create(ctx, lib))
	waitFor(t, 5*time.Minute, "KiwixLibrary Available", func(ctx context.Context) error {
		var got v1alpha1.KiwixLibrary
		if err := e.c.Get(ctx, client.ObjectKeyFromObject(lib), &got); err != nil {
			return err
		}
		return available(got.Status.Conditions)
	})
}

func (e *env) crawlServedAndNotified(t *testing.T) {
	e.diagnoseOnFailure(t)
	ctx := t.Context()
	require.NoError(t, e.c.Create(ctx, siteArchive()))

	za := e.waitAvailable(t, siteName, 15*time.Minute)
	require.NotNil(t, za.Status.Current)
	waitFor(t, 2*time.Minute, "kiwix-serve lists the crawled book", func(ctx context.Context) error {
		return e.served(ctx, za.Status.Current.BookID)
	})
	waitFor(t, 2*time.Minute, "the success notification", func(ctx context.Context) error {
		return e.notified(ctx, siteName+" updated")
	})
}

func (e *env) catalogDownload(t *testing.T) {
	e.diagnoseOnFailure(t)
	ctx := t.Context()
	book := &v1alpha1.ZimArchive{
		ObjectMeta: metav1.ObjectMeta{Name: bookName, Namespace: ns},
		Spec: v1alpha1.ZimArchiveSpec{
			LibraryRef:     corev1.LocalObjectReference{Name: libraryName},
			Source:         v1alpha1.SourceSpec{Download: &v1alpha1.DownloadSource{Catalog: &v1alpha1.CatalogRef{Name: catalogBook}}},
			Refresh:        gateDefaults(),
			DeletionPolicy: v1alpha1.DeletionPolicyDelete,
		},
	}
	require.NoError(t, e.c.Create(ctx, book))

	za := e.waitAvailable(t, bookName, 5*time.Minute)
	require.NotNil(t, za.Status.Current)
	require.Equal(t, catalogVersion, za.Status.Current.SourceVersion)
	require.Equal(t, fixtureUUID(t, "book-01.zim"), za.Status.Current.BookID)
	waitFor(t, 2*time.Minute, "kiwix-serve lists the downloaded book", func(ctx context.Context) error {
		return e.served(ctx, za.Status.Current.BookID)
	})
}

func (e *env) refreshRejected(t *testing.T) {
	e.diagnoseOnFailure(t)
	ctx := t.Context()
	before, err := e.archive(ctx, siteName)
	require.NoError(t, err)
	require.NotNil(t, before.Status.Current)

	_, err = kubectl(ctx, "apply", "-f", filepath.Join(repoRoot, "test/e2e/manifests/site-one-page.yaml"))
	require.NoError(t, err)
	waitFor(t, 3*time.Minute, "the site to serve exactly one page", func(ctx context.Context) error {
		code, body, err := e.get(ctx, ns, siteName, 80, "/index.html")
		switch {
		case err != nil:
			return err
		case code != 200 || strings.Contains(string(body), "href"):
			return fmt.Errorf("/index.html: %d, links: %t", code, strings.Contains(string(body), "href"))
		}
		if code, _, err = e.get(ctx, ns, siteName, 80, "/a.html"); err != nil {
			return err
		}
		if code != 404 {
			return fmt.Errorf("/a.html: %d", code)
		}
		return nil
	})

	za := e.finishRequest(t, siteName, 15*time.Minute)
	a := za.Status.LastAttempt
	require.Equal(t, v1alpha1.AttemptResultRejected, a.Result, a.Message)
	require.Equal(t, v1alpha1.ReasonGateRejected, a.Reason)
	require.Equal(t, before.Status.Current, za.Status.Current)
	waitFor(t, 2*time.Minute, "the rejection notification", func(ctx context.Context) error {
		return e.notified(ctx, siteName+" refresh rejected")
	})
}

func (e *env) deleteRetainKeepsFile(t *testing.T) {
	e.diagnoseOnFailure(t)
	ctx := t.Context()
	old, err := e.archive(ctx, siteName)
	require.NoError(t, err)
	require.NotNil(t, old.Status.Current)
	bookID := old.Status.Current.BookID

	require.NoError(t, e.c.Delete(ctx, old))
	waitFor(t, 3*time.Minute, "the archive to be deleted", func(ctx context.Context) error {
		switch _, err := e.archive(ctx, siteName); {
		case apierrors.IsNotFound(err):
			return nil
		case err != nil:
			return err
		}
		return errors.New("still present")
	})
	require.NoError(t, e.served(ctx, bookID), "the retained book is still served")

	require.NoError(t, e.c.Create(ctx, siteArchive()))
	za := e.waitAvailable(t, siteName, 5*time.Minute)
	require.NotNil(t, za.Status.Current)
	require.Equal(t, bookID, za.Status.Current.BookID)
	holdFor(t, 30*time.Second, "the re-created archive starts no attempt", func(ctx context.Context) error {
		got, err := e.archive(ctx, siteName)
		if err != nil {
			return err
		}
		if got.Status.LastAttempt != nil {
			return fmt.Errorf("attempt started: %+v", *got.Status.LastAttempt)
		}
		var jobs batchv1.JobList
		if err := e.c.List(ctx, &jobs, client.InNamespace(ns), client.MatchingLabels{v1alpha1.LabelArchive: siteName}); err != nil {
			return err
		}
		for _, j := range jobs.Items {
			if metav1.IsControlledBy(&j, got) {
				return fmt.Errorf("job %s created", j.Name)
			}
		}
		return nil
	})
}

func (e *env) crawlEgressBlockedWithoutFlag(t *testing.T) {
	e.diagnoseOnFailure(t)
	ctx := t.Context()
	policy := client.ObjectKey{Namespace: ns, Name: libraryName + "-crawl"}
	var np networkingv1.NetworkPolicy
	require.NoError(t, e.c.Get(ctx, policy, &np))
	require.NotEmpty(t, allowCIDRs(&np), "the crawl policy allows the e2e CIDRs before the upgrade")

	subnet, err := exec.CommandContext(ctx, "docker", "network", "inspect", "kind", "-f", "{{(index .IPAM.Config 0).Subnet}}").Output()
	require.NoError(t, err)
	_, err = run(ctx, "helm", "upgrade", "--install", "--kube-context", kindContext, release, filepath.Join(repoRoot, "chart"),
		"-n", operatorNS, "-f", filepath.Join(repoRoot, "test/e2e/values-no-egress.yaml"),
		"--set", "probeAllowCIDRs={"+strings.TrimSpace(string(subnet))+"}", "--wait", "--timeout=5m")
	require.NoError(t, err)
	waitFor(t, 3*time.Minute, "the crawl policy to drop the allow CIDRs", func(ctx context.Context) error {
		var np networkingv1.NetworkPolicy
		if err := e.c.Get(ctx, policy, &np); err != nil {
			return err
		}
		if cidrs := allowCIDRs(&np); len(cidrs) > 0 {
			return fmt.Errorf("still allows %v", cidrs)
		}
		return nil
	})

	za := e.finishRequest(t, siteName, 15*time.Minute)
	a := za.Status.LastAttempt
	require.Equal(t, v1alpha1.AttemptResultFailed, a.Result, a.Message)
	require.Equal(t, v1alpha1.ReasonCrawlFailed, a.Reason)
}

// siteArchive is the crawl of the in-cluster site, kept on deletion.
func siteArchive() *v1alpha1.ZimArchive {
	return &v1alpha1.ZimArchive{
		ObjectMeta: metav1.ObjectMeta{Name: siteName, Namespace: ns},
		Spec: v1alpha1.ZimArchiveSpec{
			LibraryRef: corev1.LocalObjectReference{Name: libraryName},
			Source: v1alpha1.SourceSpec{Crawl: &v1alpha1.CrawlSource{
				Seeds:     []string{siteURL},
				Workers:   1,
				Timeout:   metav1.Duration{Duration: 10 * time.Minute},
				KeepWarcs: true,
				Scratch:   v1alpha1.ScratchSpec{Size: resource.MustParse("1Gi")},
			}},
			Refresh:        gateDefaults(),
			Notify:         v1alpha1.ArchiveNotifySpec{OnSuccess: true},
			DeletionPolicy: v1alpha1.DeletionPolicyRetain,
		},
	}
}

// gateDefaults spells out the CRD default ratios: a typed client sends the zero ratios, which disable the gate.
func gateDefaults() v1alpha1.RefreshSpec {
	return v1alpha1.RefreshSpec{MinArticleRatio: 80, MinSizeRatio: 80}
}

func (e *env) archive(ctx context.Context, name string) (*v1alpha1.ZimArchive, error) {
	var za v1alpha1.ZimArchive
	if err := e.c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &za); err != nil {
		return nil, err
	}
	return &za, nil
}

func (e *env) waitAvailable(t *testing.T, name string, timeout time.Duration) *v1alpha1.ZimArchive {
	t.Helper()
	var za *v1alpha1.ZimArchive
	waitFor(t, timeout, name+" Available", func(ctx context.Context) error {
		got, err := e.archive(ctx, name)
		if err != nil {
			return err
		}
		za = got
		return available(got.Status.Conditions)
	})
	return za
}

// finishRequest sets a new requestedAt on archive name and waits until the attempt it started has finished.
func (e *env) finishRequest(t *testing.T, name string, timeout time.Duration) *v1alpha1.ZimArchive {
	t.Helper()
	ctx := t.Context()
	za, err := e.archive(ctx, name)
	require.NoError(t, err)
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	patch := client.MergeFrom(za.DeepCopy())
	metav1.SetMetaDataAnnotation(&za.ObjectMeta, v1alpha1.AnnotationRequestedAt, stamp)
	require.NoError(t, e.c.Patch(ctx, za, patch))

	waitFor(t, timeout, "the attempt requested at "+stamp+" to finish", func(ctx context.Context) error {
		got, err := e.archive(ctx, name)
		if err != nil {
			return err
		}
		if a := got.Status.LastAttempt; a == nil || a.RequestedAt != stamp || a.FinishedAt == nil {
			return fmt.Errorf("last attempt: %+v", a)
		}
		za = got
		return nil
	})
	return za
}

func available(conds []metav1.Condition) error {
	if c := meta.FindStatusCondition(conds, v1alpha1.ConditionAvailable); c == nil || c.Status != metav1.ConditionTrue {
		return fmt.Errorf("available condition: %+v", c)
	}
	return nil
}

// get fetches path from a Service port through the API server's service proxy; a zero code means the request failed.
func (e *env) get(ctx context.Context, namespace, svc string, port int, path string) (int, []byte, error) {
	res := e.cs.CoreV1().RESTClient().Get().Namespace(namespace).Resource("services").
		Name(fmt.Sprintf("%s:%d", svc, port)).SubResource("proxy").Suffix(path).Do(ctx)
	var code int
	res.StatusCode(&code)
	body, err := res.Raw()
	if code == 0 {
		return 0, nil, err
	}
	return code, body, nil
}

// served reports whether kiwix-serve's catalog has the book.
func (e *env) served(ctx context.Context, bookID string) error {
	code, _, err := e.get(ctx, ns, libraryName, 8080, "/catalog/v2/entry/"+bookID)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("catalog entry %s: %d", bookID, code)
	}
	return nil
}

type notification struct {
	Title   string `json:"title"`
	Message string `json:"message"`
}

// notified reports whether the sink has received a notification titled title.
func (e *env) notified(ctx context.Context, title string) error {
	code, body, err := e.get(ctx, toolsNS, "sink", 8080, "/received")
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("sink /received: %d", code)
	}
	var got []notification
	if err := json.Unmarshal(body, &got); err != nil {
		return fmt.Errorf("decode the sink's notifications: %w", err)
	}
	if !slices.ContainsFunc(got, func(n notification) bool { return n.Title == title }) {
		return fmt.Errorf("no %q notification in %+v", title, got)
	}
	return nil
}

// allowCIDRs are a policy's egress ipBlocks other than the internet rule.
func allowCIDRs(np *networkingv1.NetworkPolicy) []string {
	var out []string
	for _, r := range np.Spec.Egress {
		for _, p := range r.To {
			if p.IPBlock != nil && p.IPBlock.CIDR != "0.0.0.0/0" {
				out = append(out, p.IPBlock.CIDR)
			}
		}
	}
	return out
}

// fixtureUUID is the book ID of a fixture ZIM, from fixtures.json.
func fixtureUUID(t *testing.T, file string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, "test/fixtures/zim/fixtures.json"))
	require.NoError(t, err)
	type fixture struct {
		File string `json:"file"`
		UUID string `json:"uuid"`
	}
	var fixtures []fixture
	require.NoError(t, json.Unmarshal(data, &fixtures))
	i := slices.IndexFunc(fixtures, func(f fixture) bool { return f.File == file })
	require.GreaterOrEqual(t, i, 0, "%s is in fixtures.json", file)
	return fixtures[i].UUID
}

// waitFor polls cond until it returns nil, failing t with its last error after timeout.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func(context.Context) error) {
	t.Helper()
	var last error
	err := wait.PollUntilContextTimeout(t.Context(), poll, timeout, true, func(ctx context.Context) (bool, error) {
		last = cond(ctx)
		return last == nil, nil
	})
	if err != nil {
		t.Fatalf("waited %s for %s: %v; last: %v", timeout, what, err, last)
	}
}

// holdFor fails t as soon as cond returns an error during d.
func holdFor(t *testing.T, d time.Duration, what string, cond func(context.Context) error) {
	t.Helper()
	err := wait.PollUntilContextTimeout(t.Context(), poll, d, true, func(context.Context) (bool, error) {
		// t.Context, not the poll's, so a call in flight at the deadline is not read as a violation.
		return false, cond(t.Context())
	})
	if !wait.Interrupted(err) {
		t.Fatalf("%s: %v", what, err)
	}
}

func (e *env) diagnoseOnFailure(t *testing.T) {
	t.Cleanup(func() {
		if t.Failed() {
			e.dump(t)
		}
	})
}

// dump logs the archives, Jobs, pods and events of the test namespace, and the operator, librarian and crawl logs.
func (e *env) dump(t *testing.T) {
	e.dumped = true
	// t.Context is already cancelled when cleanups run.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, args := range [][]string{
		{"get", "kiwixlibraries,zimarchives,jobs,pods,networkpolicies", "-n", ns, "-o", "wide"},
		{"get", "zimarchives", "-n", ns, "-o", "yaml"},
		{"describe", "zimarchives,jobs,pods", "-n", ns},
		{"get", "events", "-n", ns, "--sort-by=.lastTimestamp"},
		{"logs", "-n", operatorNS, "-l", "app.kubernetes.io/name=kiwix-operator", "--tail=200"},
		{"logs", "-n", ns, "deployment/" + libraryName, "-c", "librarian", "--tail=200"},
		{"logs", "-n", ns, "-l", v1alpha1.LabelArchive, "--all-containers", "--prefix", "--tail=200"},
	} {
		out, err := kubectl(ctx, args...)
		if err != nil {
			out += "exit: " + errors.Unwrap(err).Error()
		}
		t.Logf("$ kubectl %s\n%s", strings.Join(args, " "), out)
	}
}

func kubectl(ctx context.Context, args ...string) (string, error) {
	return run(ctx, "kubectl", append([]string{"--context", kindContext}, args...)...)
}

// run runs a command and returns its combined output, which the error carries too.
func run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out), nil
}
