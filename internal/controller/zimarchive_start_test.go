package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/jacaudi/kiwix-operator/api/v1alpha1"
	"github.com/jacaudi/kiwix-operator/internal/catalog"
	"github.com/jacaudi/kiwix-operator/internal/libapi"
	"github.com/jacaudi/kiwix-operator/internal/libclient"
	"github.com/jacaudi/kiwix-operator/internal/notify"
)

var (
	// testNow is a whole second, because metav1.Time drops sub-second precision.
	testNow    = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	testSHA    = strings.Repeat("ab", 32)
	testBookID = "c22094d0-6ad1-6360-36b7-dab953595143"
)

// fakeArchiveLibrarian is the librarian of one library: an archive's current book, its registered attempts, and errors to inject.
type fakeArchiveLibrarian struct {
	current      *libapi.BookInfo
	archiveErr   error
	adoptErr     error
	adopts       []libapi.AdoptRequest
	registerErrs []error
	registered   []libapi.RegisterRequest
	getCalls     int

	// states are the attempts Get reports, ahead of the registered ones; getQueue is served first, one per Get.
	states   map[string]*libapi.AttemptState
	getQueue []libapi.AttemptState
	getErr   error

	promoteErrs []error
	promoted    []libapi.PromoteRequest
	discarded   []string

	deleteDone bool
	deleteErr  error
	deletes    []string
}

func (f *fakeArchiveLibrarian) Register(_ context.Context, req libapi.RegisterRequest) (libapi.AttemptState, error) {
	if len(f.registerErrs) > 0 {
		err := f.registerErrs[0]
		f.registerErrs = f.registerErrs[1:]
		return libapi.AttemptState{}, err
	}
	f.registered = append(f.registered, req)
	return libapi.AttemptState{Archive: req.Archive, Attempt: req.Attempt, Kind: req.Kind, State: libapi.StateRegistered}, nil
}

func (f *fakeArchiveLibrarian) Get(_ context.Context, attempt string) (libapi.AttemptState, error) {
	f.getCalls++
	if f.getErr != nil {
		return libapi.AttemptState{}, f.getErr
	}
	if len(f.getQueue) > 0 {
		st := f.getQueue[0]
		f.getQueue = f.getQueue[1:]
		return st, nil
	}
	if st, ok := f.states[attempt]; ok {
		return *st, nil
	}
	for _, r := range f.registered {
		if r.Attempt == attempt {
			return libapi.AttemptState{Archive: r.Archive, Attempt: attempt, Kind: r.Kind, State: libapi.StateRegistered}, nil
		}
	}
	return libapi.AttemptState{}, fmt.Errorf("attempt %s: %w", attempt, libclient.ErrNotFound)
}

// Promote makes the attempt's staged book current, unless an error is queued.
func (f *fakeArchiveLibrarian) Promote(_ context.Context, attempt string, req libapi.PromoteRequest) (libapi.BookInfo, error) {
	f.promoted = append(f.promoted, req)
	if len(f.promoteErrs) > 0 {
		err := f.promoteErrs[0]
		f.promoteErrs = f.promoteErrs[1:]
		return libapi.BookInfo{}, err
	}
	st := f.states[attempt]
	b := *st.Staged
	b.Archive, b.File, b.SourceVersion, b.SourceFingerprint = st.Archive, st.Archive+".zim", req.SourceVersion, req.SourceFingerprint
	f.current = &b
	st.State = libapi.StatePromoted
	return b, nil
}

func (f *fakeArchiveLibrarian) Discard(_ context.Context, attempt string) (libapi.AttemptState, error) {
	f.discarded = append(f.discarded, attempt)
	st, ok := f.states[attempt]
	if !ok {
		return libapi.AttemptState{}, fmt.Errorf("attempt %s: %w", attempt, libclient.ErrNotFound)
	}
	st.State = libapi.StateDiscarded
	return *st, nil
}

func (f *fakeArchiveLibrarian) Archive(context.Context, string) (*libapi.BookInfo, error) {
	return f.current, f.archiveErr
}

func (f *fakeArchiveLibrarian) Adopt(_ context.Context, archive string, req libapi.AdoptRequest) (libapi.BookInfo, error) {
	f.adopts = append(f.adopts, req)
	if f.adoptErr != nil {
		return libapi.BookInfo{}, f.adoptErr
	}
	f.current = &libapi.BookInfo{Archive: archive, File: archive + ".zim", BookID: testBookID, ArticleCount: 10, SizeKiB: 100, SourceVersion: "2026-10", SourceFingerprint: req.SourceFingerprint}
	return *f.current, nil
}

func (f *fakeArchiveLibrarian) Delete(_ context.Context, _, policy string) (bool, error) {
	f.deletes = append(f.deletes, policy)
	return f.deleteDone, f.deleteErr
}

// fakeCatalog serves an OPDS feed of entries and one .meta4 per entry.
type fakeCatalog struct {
	srv     *httptest.Server
	mu      sync.Mutex
	entries []string // name/flavour/version
	queries int
}

func newFakeCatalog(t *testing.T, entries ...string) *fakeCatalog {
	t.Helper()
	fc := &fakeCatalog{entries: entries}
	fc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".meta4") {
			file := strings.TrimSuffix(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], ".meta4")
			_, _ = fmt.Fprintf(w, `<metalink xmlns="urn:ietf:params:xml:ns:metalink"><file name=%q><size>4096</size><hash type="sha-256">%s</hash></file></metalink>`, file, testSHA)
			return
		}
		fc.mu.Lock()
		fc.queries++
		fc.mu.Unlock()
		var b strings.Builder
		b.WriteString(`<feed xmlns="http://www.w3.org/2005/Atom">`)
		for _, e := range fc.entries {
			p := strings.Split(e, "/")
			_, _ = fmt.Fprintf(&b, `<entry><name>%s</name><flavour>%s</flavour><link rel="http://opds-spec.org/acquisition/open-access" href="%s/zim/%s_%s_%s.zim.meta4"/></entry>`,
				p[0], p[1], fc.srv.URL, p[0], p[1], p[2])
		}
		b.WriteString(`</feed>`)
		_, _ = io.WriteString(w, b.String())
	}))
	t.Cleanup(fc.srv.Close)
	return fc
}

func (fc *fakeCatalog) count() int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.queries
}

// webhook records the titles of the notifications it receives.
type webhook struct {
	mu     sync.Mutex
	titles []string
}

func (w *webhook) got() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.titles)
}

type zaHarness struct {
	r         *ZimArchiveReconciler
	c         client.Client
	librarian *fakeArchiveLibrarian
	catalog   *fakeCatalog
	hook      *webhook
	rec       *events.FakeRecorder
}

func newZAHarness(t *testing.T, objs ...client.Object) *zaHarness {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	h := &zaHarness{librarian: &fakeArchiveLibrarian{}, catalog: newFakeCatalog(t), hook: &webhook{}, rec: events.NewFakeRecorder(20)}
	hookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Title string `json:"title"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		h.hook.mu.Lock()
		h.hook.titles = append(h.hook.titles, body.Title)
		h.hook.mu.Unlock()
	}))
	t.Cleanup(hookSrv.Close)

	lib := testLib()
	lib.Spec.Notify = &v1alpha1.NotifySpec{Webhook: v1alpha1.WebhookSpec{URL: hookSrv.URL}}
	admin := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "wiki-librarian-admin", Namespace: "kiwix"},
		Data:       map[string][]byte{"token": []byte("admin-token")},
	}
	h.c = fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(append([]client.Object{lib, admin}, objs...)...).
		WithStatusSubresource(&v1alpha1.ZimArchive{}).
		WithIndex(&v1alpha1.ZimArchive{}, libraryRefField, libraryRefOf).
		Build()
	h.r = &ZimArchiveReconciler{
		Client:        h.c,
		Scheme:        scheme,
		Recorder:      h.rec,
		OperatorImage: testImage,
		ZimitImage:    testZimitImage,
		Catalog:       &catalog.Client{BaseURL: h.catalog.srv.URL},
		Notifier:      &notify.Notifier{},
		NewLibrarian: func(lib *v1alpha1.KiwixLibrary, token string) ArchiveLibrarian {
			if lib.Name != "wiki" || token != "admin-token" {
				t.Errorf("librarian for %s with token %q, want wiki with the admin token", lib.Name, token)
			}
			return h.librarian
		},
		Now:       func() time.Time { return testNow },
		apiReader: h.c,
	}
	return h
}

func (h *zaHarness) reconcileErr(t *testing.T, name string) (ctrl.Result, error) {
	t.Helper()
	return h.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key(name)})
}

func (h *zaHarness) reconcile(t *testing.T, name string) ctrl.Result {
	t.Helper()
	res, err := h.reconcileErr(t, name)
	require.NoError(t, err)
	return res
}

func (h *zaHarness) archive(t *testing.T, name string) *v1alpha1.ZimArchive {
	t.Helper()
	var za v1alpha1.ZimArchive
	require.NoError(t, h.c.Get(t.Context(), key(name), &za))
	return &za
}

func (h *zaHarness) jobs(t *testing.T) []batchv1.Job {
	t.Helper()
	var list batchv1.JobList
	require.NoError(t, h.c.List(t.Context(), &list, client.InNamespace("kiwix")))
	return list.Items
}

func (h *zaHarness) events() []string {
	var got []string
	for len(h.rec.Events) > 0 {
		got = append(got, <-h.rec.Events)
	}
	return got
}

func assertArchiveCondition(t *testing.T, za *v1alpha1.ZimArchive, typ string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	c := meta.FindStatusCondition(za.Status.Conditions, typ)
	require.NotNil(t, c, "condition %s missing", typ)
	assert.Equal(t, status, c.Status, typ)
	assert.Equal(t, reason, c.Reason, typ)
	assert.Equal(t, za.Generation, c.ObservedGeneration, typ)
}

func stored(za *v1alpha1.ZimArchive) *v1alpha1.ZimArchive {
	za.CreationTimestamp = metav1.NewTime(testNow.Add(-72 * time.Hour))
	return za
}

func catalogArchive(name, flavour string) *v1alpha1.ZimArchive {
	return stored(&v1alpha1.ZimArchive{
		ObjectMeta: metav1.ObjectMeta{Name: "docs", Namespace: "kiwix", Generation: 1},
		Spec: v1alpha1.ZimArchiveSpec{
			LibraryRef: corev1.LocalObjectReference{Name: "wiki"},
			Source:     v1alpha1.SourceSpec{Download: &v1alpha1.DownloadSource{Catalog: &v1alpha1.CatalogRef{Name: name, Flavour: flavour}}},
			Refresh:    v1alpha1.RefreshSpec{MinArticleRatio: 80, MinSizeRatio: 80},
		},
	})
}

func urlArchive() *v1alpha1.ZimArchive {
	return stored(&v1alpha1.ZimArchive{
		ObjectMeta: metav1.ObjectMeta{Name: "file", Namespace: "kiwix", Generation: 1},
		Spec: v1alpha1.ZimArchiveSpec{
			LibraryRef: corev1.LocalObjectReference{Name: "wiki"},
			Source:     v1alpha1.SourceSpec{Download: &v1alpha1.DownloadSource{URL: "https://example.org/file_2026-10.zim"}},
		},
	})
}

func requested(za *v1alpha1.ZimArchive, value string) *v1alpha1.ZimArchive {
	za.Annotations = map[string]string{v1alpha1.AnnotationRequestedAt: value}
	return za
}

// newCurrentHarness stores za with a current version and a finished attempt, and makes the librarian report the same book.
func newCurrentHarness(t *testing.T, za *v1alpha1.ZimArchive, fingerprint string) *zaHarness {
	t.Helper()
	cur := &v1alpha1.CurrentVersion{
		File: za.Name + ".zim", BookID: testBookID, ArticleCount: 100, SizeKiB: 1000, SHA256: testSHA,
		SourceVersion: "2026-10", SourceFingerprint: fingerprint,
	}
	za.Status.Current = cur
	finished := metav1.NewTime(testNow.Add(-48 * time.Hour))
	za.Status.LastAttempt = &v1alpha1.AttemptStatus{ID: "aaaaaaaa", StartedAt: finished, FinishedAt: &finished, Result: v1alpha1.AttemptResultSucceeded}
	h := newZAHarness(t, za)
	h.librarian.current = &libapi.BookInfo{
		Archive: za.Name, File: cur.File, BookID: cur.BookID, ArticleCount: cur.ArticleCount, SizeKiB: cur.SizeKiB,
		SHA256: cur.SHA256, SourceVersion: cur.SourceVersion, SourceFingerprint: cur.SourceFingerprint,
	}
	return h
}

func TestRecreatedArchiveMirrorsCurrentNoJob(t *testing.T) {
	h := newZAHarness(t, stored(testCrawlArchive()))
	h.librarian.current = &libapi.BookInfo{
		Archive: "site", File: "site.zim", BookID: testBookID, ArticleCount: 3531, SizeKiB: 49152, SHA256: testSHA,
		SourceVersion: "2026-09", SourceFingerprint: "crawl:abc",
	}

	h.reconcile(t, "site")

	za := h.archive(t, "site")
	assert.Equal(t, &v1alpha1.CurrentVersion{
		File: "site.zim", BookID: testBookID, ArticleCount: 3531, SizeKiB: 49152, SHA256: testSHA,
		SourceVersion: "2026-09", SourceFingerprint: "crawl:abc",
	}, za.Status.Current, "an inherited book has no promotedAt")
	assert.Empty(t, h.librarian.registered)
	assert.Empty(t, h.jobs(t))
	assert.Nil(t, za.Status.LastAttempt)
	assertArchiveCondition(t, za, v1alpha1.ConditionAvailable, metav1.ConditionTrue, v1alpha1.ReasonServed)
}

func TestLibrarianUnreachable(t *testing.T) {
	h := newZAHarness(t, requested(stored(testCrawlArchive()), "r1"))
	h.librarian.archiveErr = errors.New("connection refused")

	res := h.reconcile(t, "site")

	assert.Equal(t, time.Minute, res.RequeueAfter)
	za := h.archive(t, "site")
	assertArchiveCondition(t, za, v1alpha1.ConditionAvailable, metav1.ConditionFalse, v1alpha1.ReasonLibraryUnavailable)
	assert.Nil(t, za.Status.LastAttempt)
	assert.Empty(t, za.Status.LastHandledRequestedAt)
	assert.Empty(t, h.librarian.registered)
}

func TestLibraryMissing(t *testing.T) {
	za := stored(testCrawlArchive())
	za.Spec.LibraryRef.Name = "gone"
	h := newZAHarness(t, za)

	res := h.reconcile(t, "site")

	assert.Equal(t, time.Minute, res.RequeueAfter)
	assertArchiveCondition(t, h.archive(t, "site"), v1alpha1.ConditionAvailable, metav1.ConditionFalse, v1alpha1.ReasonLibraryUnavailable)
}

func adoptingArchive(name string) *v1alpha1.ZimArchive {
	za := catalogArchive("devdocs_en_go", "")
	za.Name = name
	za.Spec.AdoptFile = "devdocs_en_go_2026-10.zim"
	return za
}

func TestAdoptionBeforeDue(t *testing.T) {
	h := newZAHarness(t, requested(adoptingArchive("docs"), "r1"))

	h.reconcile(t, "docs")

	require.Len(t, h.librarian.adopts, 1)
	assert.Equal(t, libapi.AdoptRequest{File: "devdocs_en_go_2026-10.zim", SourceFingerprint: "catalog:devdocs_en_go/"}, h.librarian.adopts[0])
	assert.Empty(t, h.librarian.registered)
	assert.Zero(t, h.catalog.count())
	za := h.archive(t, "docs")
	require.NotNil(t, za.Status.Current)
	assert.Equal(t, testBookID, za.Status.Current.BookID)
	assert.Equal(t, "docs.zim", za.Status.Current.File)
	assertArchiveCondition(t, za, v1alpha1.ConditionAvailable, metav1.ConditionTrue, v1alpha1.ReasonServed)
}

func TestAdoptMissingWaits(t *testing.T) {
	h := newZAHarness(t, requested(adoptingArchive("docs"), "r1"))
	h.librarian.adoptErr = fmt.Errorf("adopt: %w", libclient.ErrAdoptMissing)

	res := h.reconcile(t, "docs")
	h.reconcile(t, "docs")

	assert.Equal(t, time.Minute, res.RequeueAfter)
	za := h.archive(t, "docs")
	assertArchiveCondition(t, za, v1alpha1.ConditionDegraded, metav1.ConditionTrue, v1alpha1.ReasonAdoptFileMissing)
	assertArchiveCondition(t, za, v1alpha1.ConditionAvailable, metav1.ConditionFalse, v1alpha1.ReasonAwaitingAdoption)
	assert.True(t, za.Status.AdoptMissingNotified)
	assert.Equal(t, []string{"docs waiting for adoptFile"}, h.hook.got(), "notified once")
	assert.Empty(t, h.librarian.registered)
	assert.Zero(t, h.catalog.count())
}

func TestAdoptInvalidWaits(t *testing.T) {
	h := newZAHarness(t, adoptingArchive("docs"))
	h.librarian.adoptErr = fmt.Errorf("adopt: %w", libclient.ErrAdoptInvalid)

	h.reconcile(t, "docs")

	za := h.archive(t, "docs")
	assertArchiveCondition(t, za, v1alpha1.ConditionDegraded, metav1.ConditionTrue, v1alpha1.ReasonAdoptFileInvalid)
	assertArchiveCondition(t, za, v1alpha1.ConditionAvailable, metav1.ConditionFalse, v1alpha1.ReasonAwaitingAdoption)
	assert.Empty(t, h.hook.got())
	assert.Empty(t, h.librarian.registered)
}

func TestAdoptConflictTieBreak(t *testing.T) {
	h := newZAHarness(t, adoptingArchive("beta"), adoptingArchive("alpha"))

	h.reconcile(t, "beta")

	assert.Empty(t, h.librarian.adopts, "the name-order loser must not adopt")
	beta := h.archive(t, "beta")
	assertArchiveCondition(t, beta, v1alpha1.ConditionDegraded, metav1.ConditionTrue, v1alpha1.ReasonAdoptConflict)
	assertArchiveCondition(t, beta, v1alpha1.ConditionAvailable, metav1.ConditionFalse, v1alpha1.ReasonAwaitingAdoption)

	h.reconcile(t, "alpha")
	require.Len(t, h.librarian.adopts, 1, "the winner adopts")
}

func TestAdoptConflictOlderWins(t *testing.T) {
	older := adoptingArchive("zulu")
	older.CreationTimestamp = metav1.NewTime(older.CreationTimestamp.Add(-time.Hour))
	h := newZAHarness(t, older, adoptingArchive("alpha"))

	h.reconcile(t, "alpha")

	assert.Empty(t, h.librarian.adopts)
	assertArchiveCondition(t, h.archive(t, "alpha"), v1alpha1.ConditionDegraded, metav1.ConditionTrue, v1alpha1.ReasonAdoptConflict)
}

func scheduled(za *v1alpha1.ZimArchive) *v1alpha1.ZimArchive {
	za.Spec.Refresh.Schedule = "0 3 * * *"
	za.Status.LastScheduledAt = new(metav1.NewTime(time.Date(2026, 11, 23, 3, 0, 0, 0, time.UTC)))
	return za
}

func TestCatalogUnchangedConsumesTrigger(t *testing.T) {
	seed := scheduled(catalogArchive("x", "maxi"))
	h := newCurrentHarness(t, seed, "catalog:x/maxi")
	h.catalog.entries = []string{"x/maxi/2026-10"}

	h.reconcile(t, "docs")

	za := h.archive(t, "docs")
	tick := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
	require.NotNil(t, za.Status.LastScheduledAt)
	assert.True(t, tick.Equal(za.Status.LastScheduledAt.Time), "lastScheduledAt = %s, want the tick", za.Status.LastScheduledAt)
	a := za.Status.LastAttempt
	require.NotNil(t, a)
	assert.Equal(t, v1alpha1.AttemptResultUnchanged, a.Result)
	assert.Equal(t, "schedule", a.Trigger)
	assert.Empty(t, h.librarian.registered)
	assert.Equal(t, 1, h.catalog.count())

	h.reconcile(t, "docs")

	assert.Equal(t, 1, h.catalog.count(), "not due again before the next tick")
	za = h.archive(t, "docs")
	require.NotNil(t, za.Status.NextRefreshAt)
	assert.True(t, tick.Add(24*time.Hour).Equal(za.Status.NextRefreshAt.Time), "nextRefreshAt = %s", za.Status.NextRefreshAt)
	assertArchiveCondition(t, za, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonIdle)
}

func TestRequestedUnchangedConsumesRequest(t *testing.T) {
	seed := requested(catalogArchive("x", "maxi"), "r1")
	h := newCurrentHarness(t, seed, "catalog:x/maxi")
	h.catalog.entries = []string{"x/maxi/2026-09"}

	h.reconcile(t, "docs")

	za := h.archive(t, "docs")
	assert.Equal(t, "r1", za.Status.LastHandledRequestedAt)
	require.NotNil(t, za.Status.LastAttempt)
	assert.Equal(t, "requested", za.Status.LastAttempt.Trigger)
	assert.Equal(t, "r1", za.Status.LastAttempt.RequestedAt)
	assert.Equal(t, v1alpha1.AttemptResultUnchanged, za.Status.LastAttempt.Result)
	assert.Empty(t, h.librarian.registered)
}

func TestURLUnchangedBySpecSHA(t *testing.T) {
	seed := requested(urlArchive(), "r1")
	seed.Spec.Source.Download.SHA256 = testSHA
	h := newCurrentHarness(t, seed, "url:https://example.org/file_2026-10.zim")

	h.reconcile(t, "file")

	za := h.archive(t, "file")
	assert.Equal(t, v1alpha1.AttemptResultUnchanged, za.Status.LastAttempt.Result)
	assert.Empty(t, h.librarian.registered)
	assert.Zero(t, h.catalog.count())
}

func TestFingerprintChangeFetches(t *testing.T) {
	seed := requested(catalogArchive("x", "nopic"), "r1")
	h := newCurrentHarness(t, seed, "catalog:x/maxi")
	h.catalog.entries = []string{"x/maxi/2026-10", "x/nopic/2026-10"}

	res := h.reconcile(t, "docs")

	require.Len(t, h.librarian.registered, 1)
	req := h.librarian.registered[0]
	assert.Equal(t, libapi.KindFetch, req.Kind)
	assert.Equal(t, testBookID, req.ExpectedCurrentBookID)
	assert.Equal(t, h.catalog.srv.URL+"/zim/x_nopic_2026-10.zim", req.Fetch.URL)
	assert.Equal(t, 15*time.Second, res.RequeueAfter)
	za := h.archive(t, "docs")
	assert.Equal(t, "r1", za.Status.LastHandledRequestedAt)
	assertArchiveCondition(t, za, v1alpha1.ConditionProgressing, metav1.ConditionTrue, v1alpha1.ReasonDownloading)
}

func openCrawl(name string) *v1alpha1.ZimArchive {
	za := stored(testCrawlArchive())
	za.Name = name
	za.Status.LastAttempt = &v1alpha1.AttemptStatus{ID: "bbbbbbbb", Kind: libapi.KindUpload, StartedAt: metav1.NewTime(testNow.Add(-time.Hour))}
	return za
}

func TestQueuedAtMaxConcurrent(t *testing.T) {
	h := newZAHarness(t, requested(stored(testCrawlArchive()), "r1"), openCrawl("other"))

	res := h.reconcile(t, "site")

	assert.Equal(t, time.Minute, res.RequeueAfter)
	za := h.archive(t, "site")
	assertArchiveCondition(t, za, v1alpha1.ConditionProgressing, metav1.ConditionTrue, v1alpha1.ReasonQueued)
	assert.Empty(t, za.Status.LastHandledRequestedAt, "a queued request is not consumed")
	assert.Nil(t, za.Status.LastAttempt)
	assert.Empty(t, h.librarian.registered)
	assert.Empty(t, h.jobs(t))
}

func TestQueuedOnlyCountsSameKind(t *testing.T) {
	other := openCrawl("other")
	other.Status.LastAttempt.Kind = libapi.KindFetch
	h := newZAHarness(t, requested(stored(testCrawlArchive()), "r1"), other)

	h.reconcile(t, "site")

	assert.Len(t, h.librarian.registered, 1)
}

func TestQueuedExcludesSelf(t *testing.T) {
	self := openCrawl("site")
	other := openCrawl("other")
	other.Spec.LibraryRef.Name = "elsewhere"
	h := newZAHarness(t, self, other)

	n, err := h.r.openAttempts(t.Context(), self, libapi.KindUpload)

	require.NoError(t, err)
	assert.Zero(t, n, "neither the archive's own attempt nor another library's counts")
}

func TestStartOrderIdempotent(t *testing.T) {
	za := requested(stored(testCrawlArchive()), "r1")
	za.Spec.Source.Crawl.CustomCSS = ".banner{display:none}"
	h := newZAHarness(t, za)
	h.librarian.registerErrs = []error{errors.New("connection reset")}

	_, err := h.reconcileErr(t, "site")
	require.Error(t, err)

	first := h.archive(t, "site").Status.LastAttempt
	require.NotNil(t, first)
	require.Regexp(t, `^[a-z2-7]{8}$`, first.ID)
	assert.Empty(t, h.jobs(t), "no Job before registration")
	var secret corev1.Secret
	require.NoError(t, h.c.Get(t.Context(), key("site-"+first.ID+"-upload"), &secret))
	token := string(secret.Data["token"])
	require.NotEmpty(t, token)
	assert.True(t, metav1.IsControlledBy(&secret, h.archive(t, "site")))

	res := h.reconcile(t, "site")
	h.reconcile(t, "site")

	assert.Equal(t, 15*time.Second, res.RequeueAfter)
	za = h.archive(t, "site")
	assert.Equal(t, first.ID, za.Status.LastAttempt.ID)
	assert.True(t, za.Status.LastAttempt.JobCreated)
	require.Len(t, h.librarian.registered, 1)
	req := h.librarian.registered[0]
	sum := sha256.Sum256([]byte(token))
	assert.Equal(t, hex.EncodeToString(sum[:]), req.TokenHash, "the retry reuses the existing Secret")
	assert.Equal(t, first.ID, req.Attempt)
	assert.Equal(t, libapi.KindUpload, req.Kind)
	assert.Equal(t, []string{"zim"}, req.Parts)
	assert.True(t, testNow.Add(54*time.Hour+time.Hour).Equal(req.Deadline), "deadline %s: Job deadline + 1h", req.Deadline)

	jobs := h.jobs(t)
	require.Len(t, jobs, 1)
	assert.Equal(t, "site-"+first.ID, jobs[0].Name)
	assert.True(t, metav1.IsControlledBy(&jobs[0], za))
	var cm corev1.ConfigMap
	require.NoError(t, h.c.Get(t.Context(), key("site-"+first.ID+"-css"), &cm))
	assert.True(t, metav1.IsControlledBy(&cm, &jobs[0]), "the ConfigMap is owned by the Job")
	assertArchiveCondition(t, za, v1alpha1.ConditionProgressing, metav1.ConditionTrue, v1alpha1.ReasonCrawling)
}

func TestCrawlRegistersWarcsPart(t *testing.T) {
	za := requested(stored(testCrawlArchive()), "r1")
	za.Spec.Source.Crawl.KeepWarcs = true
	h := newZAHarness(t, za)

	h.reconcile(t, "site")

	require.Len(t, h.librarian.registered, 1)
	assert.Equal(t, []string{"zim", "warcs"}, h.librarian.registered[0].Parts)
}

func TestFetchRegistrationCarriesMeta4(t *testing.T) {
	h := newZAHarness(t, catalogArchive("x", "maxi"))
	h.catalog.entries = []string{"x/maxi/2026-10"}

	h.reconcile(t, "docs")

	require.Len(t, h.librarian.registered, 1)
	req := h.librarian.registered[0]
	assert.Equal(t, libapi.KindFetch, req.Kind)
	assert.Equal(t, &libapi.FetchSpec{URL: h.catalog.srv.URL + "/zim/x_maxi_2026-10.zim", SHA256: testSHA, Size: 4096}, req.Fetch)
	assert.True(t, testNow.Add(24*time.Hour).Equal(req.Deadline), "deadline %s", req.Deadline)
	assert.Empty(t, req.ExpectedCurrentBookID)
	a := h.archive(t, "docs").Status.LastAttempt
	require.NotNil(t, a)
	assert.Equal(t, req.Attempt, a.ID)
	assert.Equal(t, libapi.KindFetch, a.Kind)
	assert.Equal(t, "initial", a.Trigger)
	require.NotNil(t, a.Deadline)
	assert.True(t, testNow.Add(24*time.Hour).Equal(a.Deadline.Time))
	assert.Nil(t, a.FinishedAt)
	assert.Empty(t, h.jobs(t))
}

func TestURLFetchRegistration(t *testing.T) {
	za := urlArchive()
	za.Spec.Source.Download.SHA256 = testSHA
	h := newZAHarness(t, za)

	h.reconcile(t, "file")

	require.Len(t, h.librarian.registered, 1)
	assert.Equal(t, &libapi.FetchSpec{URL: "https://example.org/file_2026-10.zim", SHA256: testSHA}, h.librarian.registered[0].Fetch)
}

func TestCatalogErrorNoAttempt(t *testing.T) {
	h := newZAHarness(t, requested(catalogArchive("x", ""), "r1"))
	h.catalog.entries = []string{"x/maxi/2026-10", "x/nopic/2026-10"}

	res := h.reconcile(t, "docs")

	assert.Equal(t, time.Hour, res.RequeueAfter)
	za := h.archive(t, "docs")
	assertArchiveCondition(t, za, v1alpha1.ConditionDegraded, metav1.ConditionTrue, v1alpha1.ReasonCatalogLookupFailed)
	assert.Contains(t, meta.FindStatusCondition(za.Status.Conditions, v1alpha1.ConditionDegraded).Message, catalog.ErrFlavourRequired.Error())
	assert.Empty(t, za.Status.LastHandledRequestedAt)
	assert.Nil(t, za.Status.LastAttempt)
	assert.Empty(t, h.librarian.registered)
	assert.Empty(t, h.hook.got())
}

func TestCatalogRecoveryClearsDegraded(t *testing.T) {
	h := newZAHarness(t, requested(catalogArchive("x", "maxi"), "r1"))

	h.reconcile(t, "docs")
	assertArchiveCondition(t, h.archive(t, "docs"), v1alpha1.ConditionDegraded, metav1.ConditionTrue, v1alpha1.ReasonCatalogLookupFailed)

	h.catalog.entries = []string{"x/maxi/2026-10"}
	h.reconcile(t, "docs")

	assertArchiveCondition(t, h.archive(t, "docs"), v1alpha1.ConditionDegraded, metav1.ConditionFalse, v1alpha1.ReasonHealthy)
	assert.Len(t, h.librarian.registered, 1)
}

func TestInvalidSchedule(t *testing.T) {
	for _, spec := range []string{"nope", "CRON_TZ=Europe/Paris 0 3 * * *"} {
		t.Run(spec, func(t *testing.T) {
			za := requested(stored(testCrawlArchive()), "r1")
			za.Spec.Refresh.Schedule = spec
			h := newZAHarness(t, za)

			res := h.reconcile(t, "site")

			assert.Equal(t, ctrl.Result{}, res, "no requeue until the spec changes")
			za = h.archive(t, "site")
			assertArchiveCondition(t, za, v1alpha1.ConditionDegraded, metav1.ConditionTrue, v1alpha1.ReasonInvalidSchedule)
			assert.Nil(t, za.Status.LastAttempt)
			assert.Empty(t, h.librarian.registered)
			assert.Empty(t, h.jobs(t))
		})
	}
}

func TestUnchangedIsNotOpen(t *testing.T) {
	seed := requested(catalogArchive("x", "maxi"), "r1")
	h := newCurrentHarness(t, seed, "catalog:x/maxi")
	h.catalog.entries = []string{"x/maxi/2026-10"}

	h.reconcile(t, "docs")

	a := h.archive(t, "docs").Status.LastAttempt
	require.NotNil(t, a)
	assert.Empty(t, a.ID)
	require.NotNil(t, a.FinishedAt)
	assert.True(t, testNow.Equal(a.StartedAt.Time))
	assert.True(t, testNow.Equal(a.FinishedAt.Time))

	h.reconcile(t, "docs")
	assert.Zero(t, h.librarian.getCalls, "an Unchanged attempt is not open")
}

func TestMirrorClearsLostCurrent(t *testing.T) {
	seed := urlArchive()
	h := newCurrentHarness(t, seed, "url:https://example.org/file_2026-10.zim")
	h.librarian.current = nil

	h.reconcile(t, "file")

	za := h.archive(t, "file")
	assert.Nil(t, za.Status.Current)
	assert.Contains(t, h.events(), "Warning CurrentLost file.zim is no longer in the library")
	require.NotNil(t, za.Status.LastAttempt)
	assert.Equal(t, "initial", za.Status.LastAttempt.Trigger, "with current and lastAttempt cleared the archive is due as initial")
	require.Len(t, h.librarian.registered, 1)
	assert.Empty(t, h.librarian.registered[0].ExpectedCurrentBookID)
}

func TestNotDueRequeuesAtNextRefresh(t *testing.T) {
	seed := scheduled(catalogArchive("x", "maxi"))
	seed.Status.LastScheduledAt = new(metav1.NewTime(time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)))
	h := newCurrentHarness(t, seed, "catalog:x/maxi")

	res := h.reconcile(t, "docs")

	assert.Equal(t, 15*time.Hour, res.RequeueAfter, "the next tick is 2026-11-25T03:00")
	assert.Zero(t, h.catalog.count())
}

func TestNotDueWithoutScheduleRequeuesDaily(t *testing.T) {
	seed := catalogArchive("x", "maxi")
	h := newCurrentHarness(t, seed, "catalog:x/maxi")

	res := h.reconcile(t, "docs")

	assert.Equal(t, 24*time.Hour, res.RequeueAfter)
	assert.Nil(t, h.archive(t, "docs").Status.NextRefreshAt)
}

func TestPodMapsToArchive(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "site-abcdefgh-x", Namespace: "kiwix", Labels: map[string]string{v1alpha1.LabelArchive: "site"}}}
	assert.Equal(t, []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: "kiwix", Name: "site"}}}, archiveForPod(t.Context(), pod))
	assert.Empty(t, archiveForPod(t.Context(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "kiwix"}}))
}

func TestLibraryMapsToArchives(t *testing.T) {
	other := urlArchive()
	other.Spec.LibraryRef.Name = "elsewhere"
	h := newZAHarness(t, stored(testCrawlArchive()), catalogArchive("x", "maxi"), other)

	got := h.r.archivesForLibrary(t.Context(), testLib())

	assert.ElementsMatch(t, []reconcile.Request{{NamespacedName: key("site")}, {NamespacedName: key("docs")}}, got)
}

func TestQueuedCatalogArchiveSkipsCatalogLookup(t *testing.T) {
	openFetch := func(name string) *v1alpha1.ZimArchive {
		za := openCrawl(name)
		za.Status.LastAttempt.Kind = libapi.KindFetch
		return za
	}
	h := newZAHarness(t, requested(catalogArchive("x", "maxi"), "r1"), openFetch("a"), openFetch("b"))
	h.catalog.entries = []string{"x/maxi/2026-10"}

	res := h.reconcile(t, "docs")

	assert.Equal(t, time.Minute, res.RequeueAfter)
	assertArchiveCondition(t, h.archive(t, "docs"), v1alpha1.ConditionProgressing, metav1.ConditionTrue, v1alpha1.ReasonQueued)
	assert.Zero(t, h.catalog.count(), "a queued archive must not query the catalog")
}

func TestDueInputAwaitsAdoption(t *testing.T) {
	withCur := adoptingArchive("docs")
	withCur.Status.Current = &v1alpha1.CurrentVersion{BookID: testBookID}
	tests := []struct {
		name string
		za   *v1alpha1.ZimArchive
		want bool
	}{
		{name: "adoptFile and no current", za: adoptingArchive("docs"), want: true},
		{name: "adoptFile and a current", za: withCur, want: false},
		{name: "no adoptFile", za: catalogArchive("x", "maxi"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dueInput(tt.za).AwaitingAdoption

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAdoptedArchiveDoesNotRefetch(t *testing.T) {
	h := newZAHarness(t, requested(adoptingArchive("docs"), "r1"))
	h.catalog.entries = []string{"devdocs_en_go//2026-10"}

	h.reconcile(t, "docs")
	h.reconcile(t, "docs")

	assert.Empty(t, h.librarian.registered, "the adopted version is already the catalog's newest")
}
