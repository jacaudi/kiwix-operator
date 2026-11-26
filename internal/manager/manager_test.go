package manager

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	fakediscovery "k8s.io/client-go/discovery/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/jacaudi/kiwix-operator/api/v1alpha1"
	"github.com/jacaudi/kiwix-operator/internal/libclient"
)

// env is a getenv over the given key/value pairs.
func env(kv ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(k string) string { return m[k] }
}

var requiredArgs = []string{"--operator-image=op:1", "--zimit-image=zimit:1", "--operator-namespace=kiwix-system"}

func withArgs(extra ...string) []string { return append(slices.Clone(requiredArgs), extra...) }

func TestParseFlagsRequired(t *testing.T) {
	for _, flagName := range []string{"--operator-image", "--zimit-image", "--operator-namespace"} {
		t.Run(flagName, func(t *testing.T) {
			args := slices.DeleteFunc(slices.Clone(requiredArgs), func(a string) bool { return strings.HasPrefix(a, flagName+"=") })

			_, missing := ParseFlags(args, env())
			_, empty := ParseFlags(append(args, flagName+"="), env())

			require.Error(t, missing)
			assert.Contains(t, missing.Error(), flagName)
			require.Error(t, empty)
			assert.Contains(t, empty.Error(), flagName)
		})
	}
}

func TestParseFlagsReportsEveryMissingFlag(t *testing.T) {
	_, err := ParseFlags(nil, env())

	require.Error(t, err)
	for _, want := range []string{"--operator-image", "--zimit-image", "--operator-namespace"} {
		assert.Contains(t, err.Error(), want)
	}
}

func TestParseFlagsEnvFallback(t *testing.T) {
	got, err := ParseFlags(nil, env(
		"OPERATOR_IMAGE", "op:env",
		"ZIMIT_IMAGE", "zimit:env",
		"POD_NAMESPACE", "ns-env",
		"CATALOG_URL", "https://catalog.test",
		"WEBHOOK_RETRIES", "5",
		"CRAWL_EGRESS_ALLOW_CIDRS", "10.0.0.0/8, 192.168.0.0/16",
		"PROBE_ALLOW_CIDRS", "172.16.0.0/12",
	))

	require.NoError(t, err)
	assert.Equal(t, Flags{
		MetricsAddr:       ":8080",
		ProbeAddr:         ":8081",
		CatalogURL:        "https://catalog.test",
		OperatorImage:     "op:env",
		ZimitImage:        "zimit:env",
		OperatorNamespace: "ns-env",
		LogFormat:         "json",
		LogLevel:          "info",
		WebhookRetries:    5,
		EgressAllowCIDRs:  []string{"10.0.0.0/8", "192.168.0.0/16"},
		ProbeAllowCIDRs:   []string{"172.16.0.0/12"},
	}, got)
}

func TestParseFlagsFlagBeatsEnv(t *testing.T) {
	got, err := ParseFlags(withArgs("--webhook-retries=1", "--catalog-url=http://flag.test"),
		env("OPERATOR_IMAGE", "op:env", "WEBHOOK_RETRIES", "5", "CATALOG_URL", "https://env.test"))

	require.NoError(t, err)
	assert.Equal(t, "op:1", got.OperatorImage)
	assert.Equal(t, 1, got.WebhookRetries)
	assert.Equal(t, "http://flag.test", got.CatalogURL)
}

func TestParseFlagsBadCIDR(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		getenv  func(string) string
		wantErr string
	}{
		{"prefix too long", withArgs("--crawl-egress-allow-cidrs=10.0.0.0/33"), env(), "10.0.0.0/33"},
		{"not a CIDR", withArgs("--probe-allow-cidrs=10.0.0.0/8,nope"), env(), "nope"},
		{"host bits set", withArgs("--probe-allow-cidrs=10.0.0.1/8"), env(), "10.0.0.0/8"},
		{"from the environment", requiredArgs, env("CRAWL_EGRESS_ALLOW_CIDRS", "10.0.0.0/33"), "--crawl-egress-allow-cidrs"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseFlags(tc.args, tc.getenv)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestParseFlagsDefaults(t *testing.T) {
	got, err := ParseFlags(requiredArgs, env())

	require.NoError(t, err)
	assert.Equal(t, Flags{
		MetricsAddr:       ":8080",
		ProbeAddr:         ":8081",
		CatalogURL:        "https://opds.library.kiwix.org",
		OperatorImage:     "op:1",
		ZimitImage:        "zimit:1",
		OperatorNamespace: "kiwix-system",
		LogFormat:         "json",
		LogLevel:          "info",
		WebhookRetries:    3,
	}, got)
}

func TestParseFlagsChartArgs(t *testing.T) {
	got, err := ParseFlags([]string{
		"--leader-elect", "--metrics-bind-address=:8080", "--health-probe-bind-address=:8081",
		"--operator-image=ghcr.io/jacaudi/kiwix-operator:dev", "--zimit-image=x",
		"--catalog-url=https://opds.library.kiwix.org", "--webhook-retries=3",
		"--crawl-egress-allow-cidrs=", "--probe-allow-cidrs=",
	}, env("POD_NAMESPACE", "kiwix-system"))

	require.NoError(t, err)
	assert.True(t, got.LeaderElect)
	assert.Nil(t, got.EgressAllowCIDRs)
	assert.Nil(t, got.ProbeAllowCIDRs)
}

func TestParseFlagsRejectsBadValues(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		getenv  func(string) string
		wantErr string
	}{
		{"unknown flag", withArgs("--bogus"), env(), "bogus"},
		{"log format", withArgs("--log-format=xml"), env(), "log format"},
		{"log level", withArgs("--log-level=loud"), env(), "log level"},
		{"negative retries", withArgs("--webhook-retries=-1"), env(), "--webhook-retries"},
		{"retries env not a number", requiredArgs, env("WEBHOOK_RETRIES", "many"), "WEBHOOK_RETRIES"},
		{"catalog URL scheme", withArgs("--catalog-url=ftp://catalog.test"), env(), "--catalog-url"},
		{"catalog URL without host", withArgs("--catalog-url=https://"), env(), "--catalog-url"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseFlags(tc.args, tc.getenv)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestCacheOptionsSeeOnlyManagedObjects(t *testing.T) {
	opts := cacheOptions()

	managed := labels.Set{v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue}
	var filtered []string
	for obj, by := range opts.ByObject {
		filtered = append(filtered, fmt.Sprintf("%T", obj))
		require.NotNil(t, by.Label, "%T", obj)
		assert.True(t, by.Label.Matches(managed), "%T", obj)
		assert.False(t, by.Label.Matches(labels.Set{"app.kubernetes.io/managed-by": "helm"}), "%T", obj)
	}
	assert.ElementsMatch(t, []string{"*v1.Pod", "*v1.Secret", "*v1.ConfigMap"}, filtered)
}

func TestGatewayAPIAvailable(t *testing.T) {
	routes := metav1.APIResource{Name: "httproutes", Kind: "HTTPRoute"}
	tests := []struct {
		name      string
		resources []*metav1.APIResourceList
		want      bool
	}{
		{"v1 serves httproutes", []*metav1.APIResourceList{{GroupVersion: "gateway.networking.k8s.io/v1", APIResources: []metav1.APIResource{{Name: "gateways"}, routes}}}, true},
		{"group absent", nil, false},
		{"v1 without httproutes", []*metav1.APIResourceList{{GroupVersion: "gateway.networking.k8s.io/v1", APIResources: []metav1.APIResource{{Name: "gateways"}}}}, false},
		{"httproutes only in v1beta1", []*metav1.APIResourceList{{GroupVersion: "gateway.networking.k8s.io/v1beta1", APIResources: []metav1.APIResource{routes}}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{Resources: tc.resources}}

			got, err := gatewayAPIAvailable(t.Context(), d)

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestGatewayAPIAvailableFailsOnDiscoveryError(t *testing.T) {
	d := &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{}}
	d.PrependReactor("*", "*", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unreachable")
	})

	_, err := gatewayAPIAvailable(t.Context(), d)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "apiserver unreachable")
}

func wiringFlags() Flags {
	return Flags{
		CatalogURL:        "https://catalog.test",
		OperatorImage:     "op:1",
		ZimitImage:        "zimit:1",
		OperatorNamespace: "kiwix-system",
		WebhookRetries:    2,
		EgressAllowCIDRs:  []string{"10.0.0.0/8"},
		ProbeAllowCIDRs:   []string{"172.16.0.0/12"},
		UserAgent:         "kiwix-operator/v9",
	}
}

func TestNewReconcilersWiring(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	rec := events.NewFakeRecorder(1)
	f := wiringFlags()

	kl, za := newReconcilers(f, c, c.Scheme(), rec, true)

	assert.Equal(t, "op:1", kl.OperatorImage)
	assert.Equal(t, "kiwix-system", kl.OperatorNamespace)
	assert.Equal(t, []string{"10.0.0.0/8"}, kl.EgressAllowCIDRs)
	assert.Equal(t, []string{"172.16.0.0/12"}, kl.ProbeAllowCIDRs)
	assert.True(t, kl.GatewayAPIAvailable)
	assert.Same(t, rec, kl.Recorder)
	assert.Equal(t, "op:1", za.OperatorImage)
	assert.Equal(t, "zimit:1", za.ZimitImage)
	assert.Same(t, rec, za.Recorder)
	assert.Equal(t, "https://catalog.test", za.Catalog.BaseURL)
	assert.Equal(t, "kiwix-operator/v9", za.Catalog.UserAgent)
	require.NotNil(t, za.Catalog.HTTP)
	assert.Equal(t, 2, za.Notifier.Retries)
	assert.Equal(t, "kiwix-operator/v9", za.Notifier.UserAgent)

	lib := &v1alpha1.KiwixLibrary{ObjectMeta: metav1.ObjectMeta{Name: "wiki", Namespace: "media"}}
	statusClient, ok := kl.NewLibClient("http://wiki-librarian.media.svc:8090", "admin").(*libclient.Client)
	require.True(t, ok)
	archiveClient, ok := za.NewLibrarian(lib, "admin").(*libclient.Client)
	require.True(t, ok)
	assert.Equal(t, &libclient.Client{BaseURL: "http://wiki-librarian.media.svc:8090", Token: "admin", HTTP: za.Catalog.HTTP}, statusClient)
	assert.Equal(t, statusClient, archiveClient)
}

func TestNotifierReportsRedirectAsFailure(t *testing.T) {
	var followed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hook" {
			http.Redirect(w, r, "/moved", http.StatusFound)
			return
		}
		followed = true
	}))
	t.Cleanup(srv.Close)
	f := wiringFlags()
	f.WebhookRetries = 0
	c := fake.NewClientBuilder().Build()
	_, za := newReconcilers(f, c, c.Scheme(), events.NewFakeRecorder(1), false)

	err := za.Notifier.Send(t.Context(), srv.URL+"/hook", "title", "message")

	require.Error(t, err, "a redirect must not turn the POST into a GET that reports success")
	assert.False(t, followed)
}

func TestRunFailsWithoutKubeconfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", t.TempDir()+"/missing")
	t.Setenv("KUBERNETES_SERVICE_HOST", "")

	err := Run(t.Context(), wiringFlags())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kubeconfig")
}
