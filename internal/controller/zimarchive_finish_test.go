package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/jacaudi/kiwix-operator/api/v1alpha1"
	"github.com/jacaudi/kiwix-operator/internal/libapi"
	"github.com/jacaudi/kiwix-operator/internal/libclient"
	"github.com/jacaudi/kiwix-operator/internal/schedule"
	"github.com/jacaudi/kiwix-operator/internal/source"
)

const (
	newBookID   = "d33105e1-7be2-7471-47c8-ebca64606254"
	thirdBookID = "e44216f2-8cf3-8582-58d9-fcdb75717365"
)

var otherSHA = strings.Repeat("cd", 32)

// gatedSite is the crawl archive "site" with the default gate ratios.
func gatedSite() *v1alpha1.ZimArchive {
	za := stored(testCrawlArchive())
	za.Spec.Refresh.MinArticleRatio, za.Spec.Refresh.MinSizeRatio = 80, 80
	return za
}

// withOpen gives za the open attempt testAttempt of kind, started an hour ago for requestedAt r1, with a day left before its deadline.
func withOpen(za *v1alpha1.ZimArchive, kind string) *v1alpha1.ZimArchive {
	requested(za, "r1")
	za.Status.LastHandledRequestedAt = "r1"
	za.Status.LastAttempt = &v1alpha1.AttemptStatus{
		ID: testAttempt, Kind: kind, Trigger: schedule.TriggerRequested, RequestedAt: "r1",
		StartedAt: metav1.NewTime(testNow.Add(-time.Hour)), Deadline: new(metav1.NewTime(testNow.Add(23 * time.Hour))),
	}
	return za
}

func withCurrent(za *v1alpha1.ZimArchive, articles, sizeKiB int64) *v1alpha1.ZimArchive {
	za.Status.Current = &v1alpha1.CurrentVersion{
		File: za.Name + ".zim", BookID: testBookID, ArticleCount: articles, SizeKiB: sizeKiB, SHA256: testSHA, SourceVersion: "2026-10",
	}
	return za
}

// attemptIn makes the librarian report za's attempt in state, with za's current as the baseline.
func (h *zaHarness) attemptIn(za *v1alpha1.ZimArchive, state string) *libapi.AttemptState {
	st := &libapi.AttemptState{Archive: za.Name, Attempt: testAttempt, Kind: za.Status.LastAttempt.Kind, State: state}
	if cur := za.Status.Current; cur != nil {
		st.Current = &libapi.BookInfo{
			Archive: za.Name, File: cur.File, BookID: cur.BookID, ArticleCount: cur.ArticleCount, SizeKiB: cur.SizeKiB, SHA256: cur.SHA256,
		}
	}
	h.librarian.states = map[string]*libapi.AttemptState{testAttempt: st}
	return st
}

// reopen replaces name's last attempt with a new open attempt testAttempt started at at.
func (h *zaHarness) reopen(t *testing.T, name string, at time.Time) {
	t.Helper()
	za := h.archive(t, name)
	za.Status.LastAttempt = &v1alpha1.AttemptStatus{
		ID: testAttempt, Kind: za.Status.LastAttempt.Kind, Trigger: schedule.TriggerBackoff,
		StartedAt: metav1.NewTime(at), Deadline: new(metav1.NewTime(at.Add(24 * time.Hour))),
	}
	require.NoError(t, h.c.Status().Update(t.Context(), za))
}

func (h *zaHarness) at(at time.Time) { h.r.Now = func() time.Time { return at } }

// deleteLibrary leaves the KiwixLibrary terminating, held by a finalizer.
func (h *zaHarness) deleteLibrary(t *testing.T) {
	t.Helper()
	var lib v1alpha1.KiwixLibrary
	require.NoError(t, h.c.Get(t.Context(), key("wiki"), &lib))
	lib.Finalizers = []string{"test/keep"}
	require.NoError(t, h.c.Update(t.Context(), &lib))
	require.NoError(t, h.c.Delete(t.Context(), &lib))
}

func (h *zaHarness) gone(t *testing.T, name string) bool {
	t.Helper()
	var za v1alpha1.ZimArchive
	return apierrors.IsNotFound(h.c.Get(t.Context(), key(name), &za))
}

func uploadSecret(token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: uploadSecretName("site", testAttempt), Namespace: "kiwix"},
		Data:       map[string][]byte{uploadTokenKey: []byte(token)},
	}
}

// crawlJobWith is za's crawl Job of testAttempt, with cond true when cond is set.
func crawlJobWith(za *v1alpha1.ZimArchive, cond batchv1.JobConditionType) *batchv1.Job {
	job, _ := buildCrawlJob(za, testLib(), testAttempt, testZimitImage, testImage)
	if cond != "" {
		job.Status.Conditions = []batchv1.JobCondition{{Type: cond, Status: corev1.ConditionTrue, Message: "Job has reached the specified backoff limit"}}
	}
	return job
}

func crawlPod(za *v1alpha1.ZimArchive, zimit, upload corev1.ContainerState) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: jobName(za.Name, testAttempt) + "-x", Namespace: za.Namespace, Labels: crawlLabels(za, testAttempt)},
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{{Name: "zimit", State: zimit}},
			ContainerStatuses:     []corev1.ContainerStatus{{Name: "upload", State: upload}},
		},
	}
}

func running() corev1.ContainerState {
	return corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
}

func exited(code int32, msg string) corev1.ContainerState {
	return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Message: msg}}
}

func hasEvent(events []string, prefix string) bool {
	return slices.ContainsFunc(events, func(e string) bool { return strings.HasPrefix(e, prefix) })
}

// assertFinished checks that name's last attempt ended at testNow with result and reason, and returns the archive.
func assertFinished(t *testing.T, h *zaHarness, name string, result v1alpha1.AttemptResult, reason string) *v1alpha1.ZimArchive {
	t.Helper()
	za := h.archive(t, name)
	a := za.Status.LastAttempt
	require.NotNil(t, a)
	require.NotNil(t, a.FinishedAt, "attempt still open")
	assert.True(t, testNow.Equal(a.FinishedAt.Time), "finishedAt %s", a.FinishedAt)
	assert.Equal(t, result, a.Result)
	assert.Equal(t, reason, a.Reason)
	assertArchiveCondition(t, za, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonIdle)
	return za
}

func TestStagedPromotes(t *testing.T) {
	seed := withCurrent(withOpen(gatedSite(), libapi.KindUpload), 3531, 6835)
	seed.Status.LastAttempt.JobCreated = true
	h := newZAHarness(t, seed, uploadSecret("tok"), crawlJobWith(seed, batchv1.JobComplete))
	st := h.attemptIn(seed, libapi.StateStaged)
	st.Staged = &libapi.BookInfo{BookID: newBookID, ArticleCount: 3540, SizeKiB: 6900, SHA256: otherSHA}

	res := h.reconcile(t, "site")

	fingerprint := source.Fingerprint(seed.Spec.Source)
	require.Len(t, h.librarian.promoted, 1)
	assert.Equal(t, libapi.PromoteRequest{ExpectedCurrentBookID: testBookID, SourceVersion: "2026-11", SourceFingerprint: fingerprint}, h.librarian.promoted[0])
	assert.Empty(t, h.librarian.discarded)
	za := assertFinished(t, h, "site", v1alpha1.AttemptResultSucceeded, "")
	cur := za.Status.Current
	require.NotNil(t, cur)
	require.NotNil(t, cur.PromotedAt)
	assert.True(t, testNow.Equal(cur.PromotedAt.Time))
	cur.PromotedAt = nil
	assert.Equal(t, &v1alpha1.CurrentVersion{
		File: "site.zim", BookID: newBookID, ArticleCount: 3540, SizeKiB: 6900, SHA256: otherSHA, SourceVersion: "2026-11", SourceFingerprint: fingerprint,
	}, cur)
	assert.Equal(t, testAttempt, za.Status.LastAttempt.ID)
	assert.Equal(t, "3,540 pages, 6 MB (was 3,531)", za.Status.LastAttempt.Message)
	assert.Zero(t, za.Status.FailedAttempts)
	var s corev1.Secret
	assert.True(t, apierrors.IsNotFound(h.c.Get(t.Context(), key(uploadSecretName("site", testAttempt)), &s)), "upload Secret deleted")
	assertArchiveCondition(t, za, v1alpha1.ConditionAvailable, metav1.ConditionTrue, v1alpha1.ReasonServed)
	assertArchiveCondition(t, za, v1alpha1.ConditionDegraded, metav1.ConditionFalse, v1alpha1.ReasonHealthy)
	assert.True(t, hasEvent(h.events(), "Normal AttemptSucceeded"))
	assert.Empty(t, h.hook.got(), "no success notification without notify.onSuccess")
	assert.Equal(t, 24*time.Hour, res.RequeueAfter)
}

func TestRecreatedArchiveRejectsTakedownRefresh(t *testing.T) {
	h := newZAHarness(t, requested(gatedSite(), "r1"))
	h.librarian.current = &libapi.BookInfo{
		Archive: "site", File: "site.zim", BookID: testBookID, ArticleCount: 3531, SizeKiB: 49152, SHA256: testSHA,
		SourceVersion: "2026-09", SourceFingerprint: "crawl:abc",
	}

	h.reconcile(t, "site")

	a := h.archive(t, "site").Status.LastAttempt
	require.True(t, attemptOpen(a))
	require.Len(t, h.librarian.registered, 1)
	assert.Equal(t, testBookID, h.librarian.registered[0].ExpectedCurrentBookID)
	assert.True(t, hasEvent(h.events(), "Normal AttemptStarted"))
	baseline := *h.librarian.current
	h.librarian.states = map[string]*libapi.AttemptState{a.ID: {
		Archive: "site", Attempt: a.ID, Kind: libapi.KindUpload, State: libapi.StateStaged,
		Staged: &libapi.BookInfo{BookID: newBookID, ArticleCount: 12, SizeKiB: 40, SHA256: otherSHA}, Current: &baseline,
	}}

	h.reconcile(t, "site")

	assert.Equal(t, []string{a.ID}, h.librarian.discarded)
	assert.Empty(t, h.librarian.promoted)
	za := assertFinished(t, h, "site", v1alpha1.AttemptResultRejected, v1alpha1.ReasonGateRejected)
	assert.Equal(t, "12 pages vs 3,531 current (needs 80%); kept current", za.Status.LastAttempt.Message)
	assert.Equal(t, testBookID, za.Status.Current.BookID, "current kept")
	assertArchiveCondition(t, za, v1alpha1.ConditionDegraded, metav1.ConditionTrue, v1alpha1.ReasonGateRejected)
	assert.Equal(t, []string{"site refresh rejected"}, h.hook.got())
	assert.True(t, za.Status.LastAttempt.Notified)
	assert.Zero(t, za.Status.FailedAttempts, "a version is current")
	assert.True(t, hasEvent(h.events(), "Warning AttemptRejected"))
}

func TestCurrentChangedRegates(t *testing.T) {
	seed := withCurrent(withOpen(gatedSite(), libapi.KindUpload), 3531, 6835)
	h := newZAHarness(t, seed)
	st := h.attemptIn(seed, libapi.StateStaged)
	st.Staged = &libapi.BookInfo{BookID: newBookID, ArticleCount: 3540, SizeKiB: 6900}
	moved := *st
	moved.Current = &libapi.BookInfo{Archive: "site", File: "site.zim", BookID: thirdBookID, ArticleCount: 3500, SizeKiB: 6800}
	h.librarian.getQueue = []libapi.AttemptState{*st, moved}
	h.librarian.promoteErrs = []error{fmt.Errorf("promote: %w", libclient.ErrCurrentChanged)}

	h.reconcile(t, "site")

	require.Len(t, h.librarian.promoted, 2)
	assert.Equal(t, testBookID, h.librarian.promoted[0].ExpectedCurrentBookID)
	assert.Equal(t, thirdBookID, h.librarian.promoted[1].ExpectedCurrentBookID, "re-gated against the new current")
	za := assertFinished(t, h, "site", v1alpha1.AttemptResultSucceeded, "")
	assert.Equal(t, newBookID, za.Status.Current.BookID)
}

func TestCurrentChangedTwiceStaysStaged(t *testing.T) {
	seed := withCurrent(withOpen(gatedSite(), libapi.KindUpload), 3531, 6835)
	h := newZAHarness(t, seed)
	st := h.attemptIn(seed, libapi.StateStaged)
	st.Staged = &libapi.BookInfo{BookID: newBookID, ArticleCount: 3540, SizeKiB: 6900}
	changed := fmt.Errorf("promote: %w", libclient.ErrCurrentChanged)
	h.librarian.promoteErrs = []error{changed, changed}

	res := h.reconcile(t, "site")

	assert.Len(t, h.librarian.promoted, 2)
	assert.Empty(t, h.librarian.discarded)
	assert.Equal(t, 15*time.Second, res.RequeueAfter)
	za := h.archive(t, "site")
	assert.True(t, attemptOpen(za.Status.LastAttempt))
	assertArchiveCondition(t, za, v1alpha1.ConditionProgressing, metav1.ConditionTrue, v1alpha1.ReasonPromoting)
}

func TestDuplicateBookRejected(t *testing.T) {
	seed := withCurrent(withOpen(gatedSite(), libapi.KindUpload), 3531, 6835)
	h := newZAHarness(t, seed)
	st := h.attemptIn(seed, libapi.StateStaged)
	st.Staged = &libapi.BookInfo{BookID: newBookID, ArticleCount: 3540, SizeKiB: 6900}
	dup := fmt.Errorf("promote %s: %w: book %s is already served as other.zim", testAttempt, libclient.ErrDuplicateBook, newBookID)
	h.librarian.promoteErrs = []error{dup}

	h.reconcile(t, "site")

	assert.Equal(t, []string{testAttempt}, h.librarian.discarded)
	za := assertFinished(t, h, "site", v1alpha1.AttemptResultRejected, v1alpha1.ReasonDuplicateBook)
	assert.Equal(t, dup.Error(), za.Status.LastAttempt.Message)
	assertArchiveCondition(t, za, v1alpha1.ConditionDegraded, metav1.ConditionTrue, v1alpha1.ReasonDuplicateBook)
}

func TestLostRegistrationReRegisters(t *testing.T) {
	seed := withOpen(gatedSite(), libapi.KindUpload)
	h := newZAHarness(t, seed, uploadSecret("tok"))

	res := h.reconcile(t, "site")

	require.Len(t, h.librarian.registered, 1)
	req := h.librarian.registered[0]
	sum := sha256.Sum256([]byte("tok"))
	assert.Equal(t, testAttempt, req.Attempt)
	assert.Equal(t, hex.EncodeToString(sum[:]), req.TokenHash, "the Secret's token")
	assert.Equal(t, libapi.KindUpload, req.Kind)
	assert.True(t, seed.Status.LastAttempt.Deadline.Time.Equal(req.Deadline))
	za := h.archive(t, "site")
	assert.True(t, attemptOpen(za.Status.LastAttempt))
	assert.True(t, za.Status.LastAttempt.JobCreated)
	assert.Len(t, h.jobs(t), 1)
	assert.Equal(t, 15*time.Second, res.RequeueAfter)
}

func TestLostFetchRegistrationRequeriesCatalog(t *testing.T) {
	seed := withOpen(catalogArchive("x", "maxi"), libapi.KindFetch)
	seed.Status.LastAttempt.SourceVersion = "2026-10"
	h := newZAHarness(t, seed)
	h.catalog.entries = []string{"x/maxi/2026-11"}

	h.reconcile(t, "docs")

	require.Len(t, h.librarian.registered, 1)
	assert.Equal(t, h.catalog.srv.URL+"/zim/x_maxi_2026-11.zim", h.librarian.registered[0].Fetch.URL)
	assert.Equal(t, 1, h.catalog.count())
	assert.Equal(t, "2026-11", h.archive(t, "docs").Status.LastAttempt.SourceVersion, "the version now being fetched")
}

func TestLostRegistrationPastDeadlineFails(t *testing.T) {
	tests := []struct {
		name   string
		seed   *v1alpha1.ZimArchive
		kind   string
		reason string
	}{
		{name: "crawl", seed: gatedSite(), kind: libapi.KindUpload, reason: v1alpha1.ReasonUploadFailed},
		{name: "fetch", seed: urlArchive(), kind: libapi.KindFetch, reason: v1alpha1.ReasonDownloadFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seed := withOpen(tt.seed, tt.kind)
			seed.Status.LastAttempt.Deadline = new(metav1.NewTime(testNow))
			h := newZAHarness(t, seed, uploadSecret("tok"))

			h.reconcile(t, seed.Name)

			assert.Empty(t, h.librarian.registered, "never re-registered past its deadline")
			assertFinished(t, h, seed.Name, v1alpha1.AttemptResultFailed, tt.reason)
		})
	}
}

func TestJobFailedReasons(t *testing.T) {
	tests := []struct {
		name          string
		zimit, upload corev1.ContainerState
		reason, msg   string
	}{
		{name: "zimit failed", zimit: exited(1, "crawl died"), upload: corev1.ContainerState{}, reason: v1alpha1.ReasonCrawlFailed, msg: "crawl died"},
		{name: "upload failed", zimit: exited(0, ""), upload: exited(2, "upload gave up"), reason: v1alpha1.ReasonUploadFailed, msg: "upload gave up"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seed := withOpen(gatedSite(), libapi.KindUpload)
			seed.Status.LastAttempt.JobCreated = true
			h := newZAHarness(t, seed, crawlJobWith(seed, batchv1.JobFailed), crawlPod(seed, tt.zimit, tt.upload))
			h.attemptIn(seed, libapi.StateReceiving)

			h.reconcile(t, "site")

			assert.Equal(t, []string{testAttempt}, h.librarian.discarded)
			za := assertFinished(t, h, "site", v1alpha1.AttemptResultFailed, tt.reason)
			assert.Equal(t, tt.msg, za.Status.LastAttempt.Message)
			assertArchiveCondition(t, za, v1alpha1.ConditionDegraded, metav1.ConditionTrue, tt.reason)
		})
	}
}

func TestJobFailedButStagedWins(t *testing.T) {
	seed := withOpen(gatedSite(), libapi.KindUpload)
	seed.Status.LastAttempt.JobCreated = true
	h := newZAHarness(t, seed, crawlJobWith(seed, batchv1.JobFailed), crawlPod(seed, exited(0, ""), exited(1, "lost the response")))
	st := h.attemptIn(seed, libapi.StateStaged)
	st.Staged = &libapi.BookInfo{BookID: newBookID, ArticleCount: 10, SizeKiB: 100}

	h.reconcile(t, "site")

	assert.Empty(t, h.librarian.discarded)
	require.Len(t, h.librarian.promoted, 1)
	assert.Empty(t, h.librarian.promoted[0].ExpectedCurrentBookID, "first version")
	assertFinished(t, h, "site", v1alpha1.AttemptResultSucceeded, "")
}

func TestFetchFailureReasons(t *testing.T) {
	tests := []struct{ code, reason string }{
		{code: libapi.CodeHashMismatch, reason: v1alpha1.ReasonHashMismatch},
		{code: libapi.CodeInsufficientSpace, reason: v1alpha1.ReasonInsufficientSpace},
		{code: libapi.CodeInvalidZim, reason: v1alpha1.ReasonDownloadFailed},
		{code: "", reason: v1alpha1.ReasonDownloadFailed},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			seed := withOpen(urlArchive(), libapi.KindFetch)
			h := newZAHarness(t, seed)
			st := h.attemptIn(seed, libapi.StateFailed)
			st.Code, st.Error = tt.code, "boom"

			h.reconcile(t, "file")

			assert.Empty(t, h.librarian.discarded, "a failed attempt is already terminal")
			za := assertFinished(t, h, "file", v1alpha1.AttemptResultFailed, tt.reason)
			assert.Equal(t, "boom", za.Status.LastAttempt.Message)
			assert.Equal(t, int32(1), za.Status.FailedAttempts)
			assert.True(t, hasEvent(h.events(), "Warning AttemptFailed"))
		})
	}
}

func TestTriggerBookkeeping(t *testing.T) {
	t.Run("schedule", func(t *testing.T) {
		seed := withOpen(scheduled(urlArchive()), libapi.KindFetch)
		a := seed.Status.LastAttempt
		a.Trigger, a.RequestedAt = schedule.TriggerSchedule, ""
		a.ScheduleTick = seed.Status.LastScheduledAt
		h := newZAHarness(t, seed)
		h.attemptIn(seed, libapi.StateFailed)

		h.reconcile(t, "file")

		za := h.archive(t, "file")
		tick := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
		require.NotNil(t, za.Status.LastScheduledAt)
		assert.True(t, tick.Equal(za.Status.LastScheduledAt.Time), "lastScheduledAt %s: the tick that passed during the attempt", za.Status.LastScheduledAt)
		require.NotNil(t, za.Status.NextRefreshAt)
		assert.True(t, testNow.Add(time.Hour).Equal(za.Status.NextRefreshAt.Time), "nextRefreshAt %s: the 1h backoff", za.Status.NextRefreshAt)
	})
	t.Run("requested", func(t *testing.T) {
		seed := withOpen(urlArchive(), libapi.KindFetch)
		seed.Status.LastHandledRequestedAt = ""
		h := newZAHarness(t, seed)
		h.attemptIn(seed, libapi.StateFailed)

		h.reconcile(t, "file")

		za := h.archive(t, "file")
		assert.Equal(t, za.Status.LastAttempt.RequestedAt, za.Status.LastHandledRequestedAt)
		assert.Equal(t, "r1", za.Status.LastHandledRequestedAt)
	})
}

func TestRequestedMidAttemptStaysDue(t *testing.T) {
	seed := requested(withOpen(urlArchive(), libapi.KindFetch), "r2")
	h := newZAHarness(t, seed)
	h.attemptIn(seed, libapi.StateFailed)

	h.reconcile(t, "file")

	za := h.archive(t, "file")
	assert.Equal(t, "r1", za.Status.LastHandledRequestedAt, "the attempt's requestedAt, not the live annotation")
	require.NotNil(t, za.Status.NextRefreshAt)
	assert.True(t, testNow.Equal(za.Status.NextRefreshAt.Time), "due again now")

	h.reconcile(t, "file")

	require.Len(t, h.librarian.registered, 1)
	a := h.archive(t, "file").Status.LastAttempt
	assert.Equal(t, schedule.TriggerRequested, a.Trigger)
	assert.Equal(t, "r2", a.RequestedAt)
}

func TestNotifyOncePerAttempt(t *testing.T) {
	seed := withOpen(urlArchive(), libapi.KindFetch)
	h := newZAHarness(t, seed)
	h.attemptIn(seed, libapi.StateFailed).Error = "boom"

	h.reconcile(t, "file")
	h.reconcile(t, "file")

	assert.Equal(t, []string{"file failed"}, h.hook.got())
	za := h.archive(t, "file")
	assert.True(t, za.Status.LastAttempt.Notified)
	require.NotNil(t, za.Status.LastFailureNotifiedAt)
	assert.True(t, testNow.Equal(za.Status.LastFailureNotifiedAt.Time))
}

func TestPendingNotificationSent(t *testing.T) {
	seed := stored(urlArchive())
	finished := metav1.NewTime(testNow.Add(-10 * time.Minute))
	seed.Status.FailedAttempts = 1
	seed.Status.LastAttempt = &v1alpha1.AttemptStatus{
		ID: testAttempt, Kind: libapi.KindFetch, Trigger: schedule.TriggerInitial, StartedAt: finished, FinishedAt: &finished,
		Result: v1alpha1.AttemptResultFailed, Reason: v1alpha1.ReasonDownloadFailed, Message: "boom",
	}
	h := newZAHarness(t, seed)

	h.reconcile(t, "file")

	assert.Equal(t, []string{"file failed"}, h.hook.got(), "a notification lost to a failed status write is sent again")
	za := h.archive(t, "file")
	assert.True(t, za.Status.LastAttempt.Notified)
	assert.Empty(t, h.librarian.registered, "the backoff is not over")
}

func TestNotifyFailedIsAnEvent(t *testing.T) {
	seed := withOpen(urlArchive(), libapi.KindFetch)
	h := newZAHarness(t, seed)
	var lib v1alpha1.KiwixLibrary
	require.NoError(t, h.c.Get(t.Context(), key("wiki"), &lib))
	lib.Spec.Notify.Webhook.URL = "http://127.0.0.1:1/hook"
	require.NoError(t, h.c.Update(t.Context(), &lib))
	h.attemptIn(seed, libapi.StateFailed)

	h.reconcile(t, "file")

	za := assertFinished(t, h, "file", v1alpha1.AttemptResultFailed, v1alpha1.ReasonDownloadFailed)
	assert.True(t, za.Status.LastAttempt.Notified)
	assert.Nil(t, za.Status.LastFailureNotifiedAt, "nothing was delivered")
	assert.True(t, hasEvent(h.events(), "Warning NotifyFailed"))
}

func TestFailureNotificationRateLimit(t *testing.T) {
	seed := withOpen(urlArchive(), libapi.KindFetch)
	h := newZAHarness(t, seed)
	h.attemptIn(seed, libapi.StateFailed)

	h.reconcile(t, "file")
	assert.Equal(t, []string{"file failed"}, h.hook.got(), "the first failure notifies")

	h.reopen(t, "file", testNow.Add(time.Hour))
	h.at(testNow.Add(2 * time.Hour))
	h.reconcile(t, "file")
	assert.Len(t, h.hook.got(), 1, "a second failure 2h later is rate-limited")
	za := h.archive(t, "file")
	assert.Equal(t, int32(2), za.Status.FailedAttempts)
	assert.True(t, za.Status.LastAttempt.Notified)

	h.reopen(t, "file", testNow.Add(20*time.Hour))
	h.at(testNow.Add(25 * time.Hour))
	h.reconcile(t, "file")
	assert.Len(t, h.hook.got(), 2, "25h after the last notification")
	za = h.archive(t, "file")
	require.NotNil(t, za.Status.LastFailureNotifiedAt)
	assert.True(t, testNow.Add(25*time.Hour).Equal(za.Status.LastFailureNotifiedAt.Time))

	h.reopen(t, "file", testNow.Add(25*time.Hour))
	st := h.attemptIn(seed, libapi.StateStaged)
	st.Staged = &libapi.BookInfo{BookID: newBookID, ArticleCount: 10, SizeKiB: 100}
	h.at(testNow.Add(26 * time.Hour))
	h.reconcile(t, "file")
	za = h.archive(t, "file")
	assert.Equal(t, v1alpha1.AttemptResultSucceeded, za.Status.LastAttempt.Result)
	assert.Nil(t, za.Status.LastFailureNotifiedAt, "success clears it")
	assert.Zero(t, za.Status.FailedAttempts)
	assert.Len(t, h.hook.got(), 2)
}

func TestUnchangedNeverNotifies(t *testing.T) {
	seed := withCurrent(withOpen(urlArchive(), libapi.KindFetch), 100, 1000)
	seed.Spec.Notify.OnSuccess = true
	h := newZAHarness(t, seed)
	st := h.attemptIn(seed, libapi.StateStaged)
	st.Staged = &libapi.BookInfo{BookID: testBookID, ArticleCount: 100, SizeKiB: 1000, SHA256: testSHA}

	h.reconcile(t, "file")

	assert.Equal(t, []string{testAttempt}, h.librarian.discarded)
	za := assertFinished(t, h, "file", v1alpha1.AttemptResultUnchanged, "")
	a := za.Status.LastAttempt
	assert.Empty(t, a.ID, "an Unchanged result is never open")
	assert.True(t, testNow.Equal(a.StartedAt.Time))
	assert.Empty(t, h.hook.got())
	assert.True(t, hasEvent(h.events(), "Normal AttemptUnchanged"))
}

func TestSuccessNotifiesWithOnSuccess(t *testing.T) {
	tests := []struct {
		name    string
		current bool
		want    string
	}{
		{name: "first version", want: "10 pages, 0 MB (first version)"},
		{name: "refresh", current: true, want: "10 pages, 0 MB (was 9)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seed := withOpen(urlArchive(), libapi.KindFetch)
			if tt.current {
				withCurrent(seed, 9, 90)
			}
			seed.Spec.Notify.OnSuccess = true
			h := newZAHarness(t, seed)
			st := h.attemptIn(seed, libapi.StateStaged)
			st.Staged = &libapi.BookInfo{BookID: newBookID, ArticleCount: 10, SizeKiB: 100, SHA256: otherSHA}

			h.reconcile(t, "file")

			assert.Equal(t, []string{"file updated"}, h.hook.got())
			za := h.archive(t, "file")
			assert.Equal(t, tt.want, za.Status.LastAttempt.Message)
			assert.True(t, za.Status.LastAttempt.Notified)
		})
	}
}

func TestProgressingReasons(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(t *testing.T) (*zaHarness, string)
		reason string
	}{
		{name: "zimit running", reason: v1alpha1.ReasonCrawling, setup: func(t *testing.T) (*zaHarness, string) {
			seed := withOpen(gatedSite(), libapi.KindUpload)
			seed.Status.LastAttempt.JobCreated = true
			h := newZAHarness(t, seed, crawlJobWith(seed, ""), crawlPod(seed, running(), corev1.ContainerState{}))
			h.attemptIn(seed, libapi.StateRegistered)
			return h, "site"
		}},
		{name: "no pod yet", reason: v1alpha1.ReasonCrawling, setup: func(t *testing.T) (*zaHarness, string) {
			seed := withOpen(gatedSite(), libapi.KindUpload)
			seed.Status.LastAttempt.JobCreated = true
			h := newZAHarness(t, seed, crawlJobWith(seed, ""))
			h.attemptIn(seed, libapi.StateRegistered)
			return h, "site"
		}},
		{name: "upload running", reason: v1alpha1.ReasonUploading, setup: func(t *testing.T) (*zaHarness, string) {
			seed := withOpen(gatedSite(), libapi.KindUpload)
			seed.Status.LastAttempt.JobCreated = true
			h := newZAHarness(t, seed, crawlJobWith(seed, ""), crawlPod(seed, exited(0, ""), running()))
			h.attemptIn(seed, libapi.StateReceiving)
			return h, "site"
		}},
		{name: "fetch receiving", reason: v1alpha1.ReasonDownloading, setup: func(t *testing.T) (*zaHarness, string) {
			seed := withOpen(urlArchive(), libapi.KindFetch)
			h := newZAHarness(t, seed)
			h.attemptIn(seed, libapi.StateReceiving)
			return h, "file"
		}},
		{name: "promoting", reason: v1alpha1.ReasonPromoting, setup: func(t *testing.T) (*zaHarness, string) {
			seed := withOpen(urlArchive(), libapi.KindFetch)
			h := newZAHarness(t, seed)
			h.attemptIn(seed, libapi.StatePromoting)
			return h, "file"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, name := tt.setup(t)

			res := h.reconcile(t, name)

			assert.Equal(t, 15*time.Second, res.RequeueAfter)
			za := h.archive(t, name)
			assert.True(t, attemptOpen(za.Status.LastAttempt))
			assertArchiveCondition(t, za, v1alpha1.ConditionProgressing, metav1.ConditionTrue, tt.reason)
		})
	}
}

func TestFinalizerAddedOnFirstReconcile(t *testing.T) {
	h := newZAHarness(t, stored(testCrawlArchive()))

	h.reconcile(t, "site")

	za := h.archive(t, "site")
	assert.Contains(t, za.Finalizers, v1alpha1.FinalizerName)
	assert.True(t, attemptOpen(za.Status.LastAttempt), "the reconcile continues after adding the finalizer")
}

// deleting marks za as being deleted, held by the operator's finalizer.
func deleting(za *v1alpha1.ZimArchive) *v1alpha1.ZimArchive {
	za.Finalizers = []string{v1alpha1.FinalizerName}
	za.DeletionTimestamp = new(metav1.NewTime(testNow.Add(-time.Minute)))
	return za
}

func TestFinalizerDeleteWaitsFor202(t *testing.T) {
	seed := deleting(stored(urlArchive()))
	seed.Spec.DeletionPolicy = v1alpha1.DeletionPolicyDelete
	h := newZAHarness(t, seed)

	res := h.reconcile(t, "file")

	assert.Equal(t, 10*time.Second, res.RequeueAfter)
	assert.Equal(t, []string{"Delete"}, h.librarian.deletes)
	za := h.archive(t, "file")
	assert.Contains(t, za.Finalizers, v1alpha1.FinalizerName)
	require.NotNil(t, za.Status.FinalizerFirstFailureAt)
	assert.True(t, testNow.Equal(za.Status.FinalizerFirstFailureAt.Time))

	h.librarian.deleteDone = true
	h.reconcile(t, "file")

	assert.True(t, h.gone(t, "file"), "finalizer removed once the librarian is done")
}

func TestFinalizerDefaultsToRetain(t *testing.T) {
	h := newZAHarness(t, deleting(stored(urlArchive())))
	h.librarian.deleteDone = true

	h.reconcile(t, "file")

	assert.Equal(t, []string{"Retain"}, h.librarian.deletes)
	assert.True(t, h.gone(t, "file"))
}

func TestFinalizerGivesUpAfterTenMinutes(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "pending 202"},
		{name: "librarian unreachable", err: errors.New("connection refused")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newZAHarness(t, deleting(stored(urlArchive())))
			h.librarian.deleteErr = tt.err

			h.reconcile(t, "file")
			h.at(testNow.Add(9 * time.Minute))
			res := h.reconcile(t, "file")

			assert.Equal(t, 10*time.Second, res.RequeueAfter)
			assert.False(t, h.gone(t, "file"), "kept at 9 minutes")
			require.True(t, testNow.Equal(h.archive(t, "file").Status.FinalizerFirstFailureAt.Time), "the first failure is kept")

			h.at(testNow.Add(11 * time.Minute))
			h.reconcile(t, "file")

			assert.True(t, h.gone(t, "file"), "removed after 10 minutes")
			assert.True(t, hasEvent(h.events(), "Warning FinalizerGaveUp"))
		})
	}
}

func TestFinalizerLibraryGone(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T) *zaHarness
	}{
		{name: "absent", setup: func(t *testing.T) *zaHarness {
			za := deleting(stored(urlArchive()))
			za.Spec.LibraryRef.Name = "gone"
			return newZAHarness(t, za)
		}},
		{name: "deleting", setup: func(t *testing.T) *zaHarness {
			h := newZAHarness(t, deleting(stored(urlArchive())))
			h.deleteLibrary(t)
			return h
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := tt.setup(t)

			h.reconcile(t, "file")

			assert.True(t, h.gone(t, "file"))
			assert.Empty(t, h.librarian.deletes)
			assert.True(t, hasEvent(h.events(), "Warning LibraryGone"))
		})
	}
}

func TestResumeRegisteredWithoutJob(t *testing.T) {
	seed := withOpen(gatedSite(), libapi.KindUpload)
	seed.Spec.Source.Crawl.CustomCSS = ".banner{display:none}"
	h := newZAHarness(t, seed, uploadSecret("tok"))
	h.attemptIn(seed, libapi.StateRegistered)

	h.reconcile(t, "site")

	assert.Empty(t, h.librarian.registered)
	jobs := h.jobs(t)
	require.Len(t, jobs, 1)
	assert.Equal(t, jobName("site", testAttempt), jobs[0].Name)
	var cm corev1.ConfigMap
	require.NoError(t, h.c.Get(t.Context(), key(cssConfigMapName("site", testAttempt)), &cm))
	assert.True(t, h.archive(t, "site").Status.LastAttempt.JobCreated)
}

func TestPromotedButStatusLost(t *testing.T) {
	seed := withCurrent(withOpen(gatedSite(), libapi.KindUpload), 3531, 6835)
	h := newZAHarness(t, seed)
	h.attemptIn(seed, libapi.StatePromoted)
	h.librarian.current = &libapi.BookInfo{Archive: "site", File: "site.zim", BookID: newBookID, ArticleCount: 3540, SizeKiB: 6900, SourceVersion: "2026-11"}

	h.reconcile(t, "site")

	assert.Empty(t, h.librarian.promoted)
	za := assertFinished(t, h, "site", v1alpha1.AttemptResultSucceeded, "")
	assert.Equal(t, newBookID, za.Status.Current.BookID)
	assert.Equal(t, "2026-11", za.Status.Current.SourceVersion)
}

func TestDiscardedByLibrarian(t *testing.T) {
	seed := withOpen(gatedSite(), libapi.KindUpload)
	h := newZAHarness(t, seed)
	h.attemptIn(seed, libapi.StateDiscarded).Error = "deadline passed"

	h.reconcile(t, "site")

	assert.Empty(t, h.librarian.discarded)
	za := assertFinished(t, h, "site", v1alpha1.AttemptResultFailed, v1alpha1.ReasonUploadFailed)
	assert.Equal(t, "discarded by the librarian", za.Status.LastAttempt.Message)
}

func TestPromotingPastDeadline(t *testing.T) {
	seed := withOpen(gatedSite(), libapi.KindUpload)
	seed.Status.LastAttempt.Deadline = new(metav1.NewTime(testNow.Add(-time.Minute)))
	h := newZAHarness(t, seed)
	h.attemptIn(seed, libapi.StatePromoting)

	h.reconcile(t, "site")

	assert.Equal(t, []string{testAttempt}, h.librarian.discarded)
	assertFinished(t, h, "site", v1alpha1.AttemptResultFailed, v1alpha1.ReasonUploadFailed)
}

func TestMissingSecretRecreated(t *testing.T) {
	seed := withOpen(gatedSite(), libapi.KindUpload)
	h := newZAHarness(t, seed)

	h.reconcile(t, "site")

	var s corev1.Secret
	require.NoError(t, h.c.Get(t.Context(), key(uploadSecretName("site", testAttempt)), &s))
	token := s.Data[uploadTokenKey]
	require.NotEmpty(t, token)
	require.Len(t, h.librarian.registered, 1)
	sum := sha256.Sum256(token)
	assert.Equal(t, hex.EncodeToString(sum[:]), h.librarian.registered[0].TokenHash)
}

func TestTickDuringAttemptSkipped(t *testing.T) {
	seed := withCurrent(withOpen(scheduled(urlArchive()), libapi.KindFetch), 100, 1000)
	h := newZAHarness(t, seed)
	st := h.attemptIn(seed, libapi.StateReceiving)

	h.reconcile(t, "file")

	tick := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
	assert.True(t, hasEvent(h.events(), "Warning ScheduleSkipped"))
	za := h.archive(t, "file")
	assert.True(t, tick.Equal(za.Status.LastScheduledAt.Time), "lastScheduledAt %s", za.Status.LastScheduledAt)

	st.State, st.Staged = libapi.StateStaged, &libapi.BookInfo{BookID: newBookID, ArticleCount: 100, SizeKiB: 1000, SHA256: otherSHA}
	h.reconcile(t, "file")
	assertFinished(t, h, "file", v1alpha1.AttemptResultSucceeded, "")
	assert.False(t, hasEvent(h.events(), "Warning ScheduleSkipped"), "the tick is skipped once")

	h.reconcile(t, "file")

	assert.Empty(t, h.librarian.registered, "the skipped tick does not start an attempt after the finish")
}

func TestScheduledAttemptNoFalseSkip(t *testing.T) {
	seed := withOpen(scheduled(urlArchive()), libapi.KindFetch)
	tick := metav1.NewTime(time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC))
	a := seed.Status.LastAttempt
	a.Trigger, a.RequestedAt, a.ScheduleTick, a.StartedAt = schedule.TriggerSchedule, "", &tick, tick
	seed.Status.LastScheduledAt = &tick
	h := newZAHarness(t, seed)
	h.attemptIn(seed, libapi.StateReceiving)

	h.reconcile(t, "file")

	assert.False(t, hasEvent(h.events(), "Warning ScheduleSkipped"))
	assert.True(t, tick.Equal(h.archive(t, "file").Status.LastScheduledAt))
}

func TestLibraryGoneDuringAttempt(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T) *zaHarness
	}{
		{name: "absent", setup: func(t *testing.T) *zaHarness {
			za := withOpen(urlArchive(), libapi.KindFetch)
			za.Spec.LibraryRef.Name = "gone"
			return newZAHarness(t, za)
		}},
		{name: "deleting", setup: func(t *testing.T) *zaHarness {
			h := newZAHarness(t, withOpen(urlArchive(), libapi.KindFetch))
			h.deleteLibrary(t)
			return h
		}},
		{name: "admin Secret missing", setup: func(t *testing.T) *zaHarness {
			h := newZAHarness(t, withOpen(urlArchive(), libapi.KindFetch))
			require.NoError(t, h.c.Delete(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "wiki-librarian-admin", Namespace: "kiwix"}}))
			return h
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := tt.setup(t)

			h.reconcile(t, "file")

			assert.Zero(t, h.librarian.getCalls, "no librarian call")
			za := assertFinished(t, h, "file", v1alpha1.AttemptResultFailed, v1alpha1.ReasonLibraryUnavailable)
			assertArchiveCondition(t, za, v1alpha1.ConditionAvailable, metav1.ConditionFalse, v1alpha1.ReasonLibraryUnavailable)
		})
	}
}

func TestLibrarianUnreachableUntilDeadline(t *testing.T) {
	seed := withOpen(urlArchive(), libapi.KindFetch)
	h := newZAHarness(t, seed)
	h.librarian.getErr = errors.New("connection refused")
	deadline := seed.Status.LastAttempt.Deadline.Time

	res := h.reconcile(t, "file")

	assert.Equal(t, time.Minute, res.RequeueAfter)
	za := h.archive(t, "file")
	assert.True(t, attemptOpen(za.Status.LastAttempt))
	assertArchiveCondition(t, za, v1alpha1.ConditionAvailable, metav1.ConditionFalse, v1alpha1.ReasonLibraryUnavailable)

	h.at(deadline.Add(59 * time.Minute))
	h.reconcile(t, "file")
	assert.True(t, attemptOpen(h.archive(t, "file").Status.LastAttempt), "kept open until an hour past the deadline")

	h.at(deadline.Add(61 * time.Minute))
	h.reconcile(t, "file")
	za = h.archive(t, "file")
	require.NotNil(t, za.Status.LastAttempt.FinishedAt)
	assert.Equal(t, v1alpha1.AttemptResultFailed, za.Status.LastAttempt.Result)
	assert.Equal(t, v1alpha1.ReasonLibraryUnavailable, za.Status.LastAttempt.Reason)
}

func TestJobGoneAfterTTL(t *testing.T) {
	seed := withOpen(gatedSite(), libapi.KindUpload)
	seed.Status.LastAttempt.JobCreated = true
	h := newZAHarness(t, seed)
	h.attemptIn(seed, libapi.StateReceiving)

	h.reconcile(t, "site")

	assert.Equal(t, []string{testAttempt}, h.librarian.discarded)
	assert.Empty(t, h.jobs(t), "the Job is not re-created")
	za := assertFinished(t, h, "site", v1alpha1.AttemptResultFailed, v1alpha1.ReasonUploadFailed)
	assert.Equal(t, "crawl Job gone", za.Status.LastAttempt.Message)
}

func TestCatalogAttemptKeepsItsVersion(t *testing.T) {
	h := newZAHarness(t, catalogArchive("x", "maxi"))
	h.catalog.entries = []string{"x/maxi/2026-10"}

	h.reconcile(t, "docs")

	a := h.archive(t, "docs").Status.LastAttempt
	require.True(t, attemptOpen(a))
	assert.Equal(t, "2026-10", a.SourceVersion)
	h.catalog.entries = []string{"x/maxi/2026-11"}
	h.librarian.states = map[string]*libapi.AttemptState{a.ID: {
		Archive: "docs", Attempt: a.ID, Kind: libapi.KindFetch, State: libapi.StateStaged,
		Staged: &libapi.BookInfo{BookID: newBookID, ArticleCount: 10, SizeKiB: 100},
	}}

	h.reconcile(t, "docs")

	require.Len(t, h.librarian.promoted, 1)
	assert.Equal(t, "2026-10", h.librarian.promoted[0].SourceVersion, "the version fetched, though the catalog has moved on")
	assert.Equal(t, 1, h.catalog.count(), "an open attempt never re-queries the catalog")
}

func TestPromoteSourceVersion(t *testing.T) {
	tests := []struct {
		name string
		za   *v1alpha1.ZimArchive
		want string
	}{
		{name: "crawl", za: testCrawlArchive(), want: "2026-11"},
		{name: "versioned url", za: urlArchive(), want: "2026-10"},
		{name: "url with a query", za: urlWith("https://example.org/f_2026-09.zim?sig=abc"), want: "2026-09"},
		{name: "unversioned url", za: urlWith("https://example.org/latest.zim"), want: "2026-11"},
		{name: "catalog", za: catalogWithAttemptVersion("2026-09"), want: "2026-09"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, promoteSourceVersion(tt.za, testNow))
		})
	}
}

func urlWith(u string) *v1alpha1.ZimArchive {
	za := urlArchive()
	za.Spec.Source.Download.URL = u
	return za
}

func catalogWithAttemptVersion(v string) *v1alpha1.ZimArchive {
	za := withOpen(catalogArchive("x", "maxi"), libapi.KindFetch)
	za.Status.LastAttempt.SourceVersion = v
	return za
}

func TestConditionTable(t *testing.T) {
	failedBefore := func(za *v1alpha1.ZimArchive, reason string) {
		finished := metav1.NewTime(testNow.Add(-48 * time.Hour))
		za.Status.LastAttempt = &v1alpha1.AttemptStatus{
			ID: "aaaaaaaa", Kind: libapi.KindFetch, StartedAt: finished, FinishedAt: &finished,
			Result: v1alpha1.AttemptResultFailed, Reason: reason, Notified: true,
		}
	}
	degraded := func(za *v1alpha1.ZimArchive, reason string) {
		za.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionDegraded, Status: metav1.ConditionTrue, Reason: reason, LastTransitionTime: metav1.NewTime(testNow)}}
	}
	tests := []struct {
		name   string
		setup  func(t *testing.T) (*zaHarness, string)
		typ    string
		status metav1.ConditionStatus
		reason string
	}{
		{
			name: "served", typ: v1alpha1.ConditionAvailable, status: metav1.ConditionTrue, reason: v1alpha1.ReasonServed,
			setup: func(t *testing.T) (*zaHarness, string) {
				return newCurrentHarness(t, urlArchive(), "url:https://example.org/file_2026-10.zim"), "file"
			},
		},
		{
			name: "librarian unreachable beats served", typ: v1alpha1.ConditionAvailable, status: metav1.ConditionFalse, reason: v1alpha1.ReasonLibraryUnavailable,
			setup: func(t *testing.T) (*zaHarness, string) {
				h := newCurrentHarness(t, urlArchive(), "url:https://example.org/file_2026-10.zim")
				h.librarian.archiveErr = errors.New("connection refused")
				return h, "file"
			},
		},
		{
			name: "library deleting beats served", typ: v1alpha1.ConditionAvailable, status: metav1.ConditionFalse, reason: v1alpha1.ReasonLibraryUnavailable,
			setup: func(t *testing.T) (*zaHarness, string) {
				h := newCurrentHarness(t, urlArchive(), "url:https://example.org/file_2026-10.zim")
				h.deleteLibrary(t)
				return h, "file"
			},
		},
		{
			name: "awaiting adoption", typ: v1alpha1.ConditionAvailable, status: metav1.ConditionFalse, reason: v1alpha1.ReasonAwaitingAdoption,
			setup: func(t *testing.T) (*zaHarness, string) {
				h := newZAHarness(t, adoptingArchive("docs"))
				h.librarian.adoptErr = fmt.Errorf("adopt: %w", libclient.ErrAdoptMissing)
				return h, "docs"
			},
		},
		{
			name: "not yet fetched", typ: v1alpha1.ConditionAvailable, status: metav1.ConditionFalse, reason: v1alpha1.ReasonNotYetFetched,
			setup: func(t *testing.T) (*zaHarness, string) {
				seed := withOpen(urlArchive(), libapi.KindFetch)
				h := newZAHarness(t, seed)
				h.attemptIn(seed, libapi.StateReceiving)
				return h, "file"
			},
		},
		{
			name: "healthy before any attempt", typ: v1alpha1.ConditionDegraded, status: metav1.ConditionFalse, reason: v1alpha1.ReasonHealthy,
			setup: func(t *testing.T) (*zaHarness, string) {
				return newZAHarness(t, stored(testCrawlArchive())), "site"
			},
		},
		{
			name: "failed attempt", typ: v1alpha1.ConditionDegraded, status: metav1.ConditionTrue, reason: v1alpha1.ReasonInsufficientSpace,
			setup: func(t *testing.T) (*zaHarness, string) {
				seed := withOpen(urlArchive(), libapi.KindFetch)
				h := newZAHarness(t, seed)
				h.attemptIn(seed, libapi.StateFailed).Code = libapi.CodeInsufficientSpace
				return h, "file"
			},
		},
		{
			name: "rejected attempt", typ: v1alpha1.ConditionDegraded, status: metav1.ConditionTrue, reason: v1alpha1.ReasonGateRejected,
			setup: func(t *testing.T) (*zaHarness, string) {
				seed := withCurrent(withOpen(gatedSite(), libapi.KindUpload), 100, 1000)
				h := newZAHarness(t, seed)
				h.attemptIn(seed, libapi.StateStaged).Staged = &libapi.BookInfo{BookID: newBookID, ArticleCount: 10, SizeKiB: 1000}
				return h, "site"
			},
		},
		{
			name: "kept while an attempt is open", typ: v1alpha1.ConditionDegraded, status: metav1.ConditionTrue, reason: v1alpha1.ReasonCrawlFailed,
			setup: func(t *testing.T) (*zaHarness, string) {
				seed := withOpen(urlArchive(), libapi.KindFetch)
				degraded(seed, v1alpha1.ReasonCrawlFailed)
				h := newZAHarness(t, seed)
				h.attemptIn(seed, libapi.StateReceiving)
				return h, "file"
			},
		},
		{
			name: "held reason beats the attempt's", typ: v1alpha1.ConditionDegraded, status: metav1.ConditionTrue, reason: v1alpha1.ReasonInvalidSchedule,
			setup: func(t *testing.T) (*zaHarness, string) {
				seed := withOpen(urlArchive(), libapi.KindFetch)
				seed.Spec.Refresh.Schedule = "nope"
				h := newZAHarness(t, seed)
				h.attemptIn(seed, libapi.StateFailed)
				return h, "file"
			},
		},
		{
			name: "resolved held reason falls back to the attempt's", typ: v1alpha1.ConditionDegraded, status: metav1.ConditionTrue, reason: v1alpha1.ReasonDownloadFailed,
			setup: func(t *testing.T) (*zaHarness, string) {
				seed := urlArchive()
				h := newCurrentHarness(t, seed, "url:https://example.org/file_2026-10.zim")
				za := h.archive(t, "file")
				failedBefore(za, v1alpha1.ReasonDownloadFailed)
				degraded(za, v1alpha1.ReasonInvalidSchedule)
				require.NoError(t, h.c.Status().Update(t.Context(), za))
				return h, "file"
			},
		},
		{
			name: "unchanged clears an earlier failure", typ: v1alpha1.ConditionDegraded, status: metav1.ConditionFalse, reason: v1alpha1.ReasonHealthy,
			setup: func(t *testing.T) (*zaHarness, string) {
				h := newCurrentHarness(t, requested(catalogArchive("x", "maxi"), "r1"), "catalog:x/maxi")
				h.catalog.entries = []string{"x/maxi/2026-10"}
				za := h.archive(t, "docs")
				failedBefore(za, v1alpha1.ReasonDownloadFailed)
				degraded(za, v1alpha1.ReasonDownloadFailed)
				require.NoError(t, h.c.Status().Update(t.Context(), za))
				return h, "docs"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, name := tt.setup(t)

			h.reconcile(t, name)

			za := h.archive(t, name)
			assertArchiveCondition(t, za, tt.typ, tt.status, tt.reason)
			assert.Equal(t, za.Generation, za.Status.ObservedGeneration)
			for _, typ := range []string{v1alpha1.ConditionAvailable, v1alpha1.ConditionProgressing, v1alpha1.ConditionDegraded} {
				assert.NotNil(t, meta.FindStatusCondition(za.Status.Conditions, typ), "%s set on every reconcile", typ)
			}
		})
	}
}

// intercept routes the reconciler's client through funcs; its API reader stays on the plain fake.
func (h *zaHarness) intercept(t *testing.T, funcs interceptor.Funcs) {
	t.Helper()
	wc, ok := h.c.(client.WithWatch)
	require.True(t, ok, "the fake client is a WithWatch")
	h.r.Client = interceptor.NewClient(wc, funcs)
}

// failStatusWriteAfterDiscard fails the first ZimArchive status write once the librarian has discarded an attempt.
func (h *zaHarness) failStatusWriteAfterDiscard(t *testing.T) {
	t.Helper()
	failed := false
	h.intercept(t, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if !failed && len(h.librarian.discarded) > 0 {
				failed = true
				return apierrors.NewConflict(schema.GroupResource{Group: "kiwix.jacaudi.dev", Resource: "zimarchives"}, obj.GetName(), errors.New("stale resourceVersion"))
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
}

func TestDiscardedOutcomeSurvivesLostStatusWrite(t *testing.T) {
	t.Run("unchanged", func(t *testing.T) {
		seed := withCurrent(withOpen(urlArchive(), libapi.KindFetch), 100, 1000)
		seed.Spec.Notify.OnSuccess = true
		h := newZAHarness(t, seed)
		h.attemptIn(seed, libapi.StateStaged).Staged = &libapi.BookInfo{BookID: testBookID, ArticleCount: 100, SizeKiB: 1000, SHA256: testSHA}
		h.failStatusWriteAfterDiscard(t)

		_, err := h.reconcileErr(t, "file")
		require.Error(t, err, "the status write after Discard fails")
		h.reconcile(t, "file")

		za := assertFinished(t, h, "file", v1alpha1.AttemptResultUnchanged, "")
		assert.Empty(t, za.Status.LastAttempt.ID)
		assert.Equal(t, "download matches current; nothing to promote", za.Status.LastAttempt.Message)
		assert.Empty(t, h.hook.got(), "Unchanged never notifies")
		assertArchiveCondition(t, za, v1alpha1.ConditionDegraded, metav1.ConditionFalse, v1alpha1.ReasonHealthy)
	})
	t.Run("gate rejected", func(t *testing.T) {
		seed := withCurrent(withOpen(gatedSite(), libapi.KindUpload), 3531, 6835)
		h := newZAHarness(t, seed)
		h.attemptIn(seed, libapi.StateStaged).Staged = &libapi.BookInfo{BookID: newBookID, ArticleCount: 12, SizeKiB: 40}
		h.failStatusWriteAfterDiscard(t)

		_, err := h.reconcileErr(t, "site")
		require.Error(t, err, "the status write after Discard fails")
		h.reconcile(t, "site")

		za := assertFinished(t, h, "site", v1alpha1.AttemptResultRejected, v1alpha1.ReasonGateRejected)
		assert.Equal(t, "12 pages vs 3,531 current (needs 80%); kept current", za.Status.LastAttempt.Message)
		assert.Equal(t, []string{"site refresh rejected"}, h.hook.got())
		assertArchiveCondition(t, za, v1alpha1.ConditionDegraded, metav1.ConditionTrue, v1alpha1.ReasonGateRejected)
	})
}

func TestPendingOutcomeKeepsAttemptOpen(t *testing.T) {
	seed := withCurrent(withOpen(gatedSite(), libapi.KindUpload), 3531, 6835)
	h := newZAHarness(t, seed)
	h.attemptIn(seed, libapi.StateStaged).Staged = &libapi.BookInfo{BookID: newBookID, ArticleCount: 12, SizeKiB: 40}
	h.failStatusWriteAfterDiscard(t)

	_, err := h.reconcileErr(t, "site")
	require.Error(t, err)

	za := h.archive(t, "site")
	a := za.Status.LastAttempt
	assert.True(t, attemptOpen(a), "a pending outcome does not finish the attempt")
	assert.Equal(t, v1alpha1.AttemptResultRejected, a.Result, "the decision is recorded before Discard")
	assert.False(t, a.Notified)
	assert.Empty(t, h.hook.got())
	assert.False(t, dueInput(za).LastFailedOrRejected, "an open attempt's pending result is not a finished failure")
}

func TestJobNotYetCachedIsNotGone(t *testing.T) {
	seed := withOpen(gatedSite(), libapi.KindUpload)
	seed.Status.LastAttempt.JobCreated = true
	h := newZAHarness(t, seed, crawlJobWith(seed, ""))
	h.attemptIn(seed, libapi.StateRegistered)
	h.intercept(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*batchv1.Job); ok {
				return apierrors.NewNotFound(schema.GroupResource{Group: "batch", Resource: "jobs"}, key.Name)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	res := h.reconcile(t, "site")

	assert.Empty(t, h.librarian.discarded, "a Job missing only from the cache is not gone")
	assert.True(t, attemptOpen(h.archive(t, "site").Status.LastAttempt))
	assert.Equal(t, 15*time.Second, res.RequeueAfter)
}

func TestCrawlJobEndedWithInsufficientSpace(t *testing.T) {
	seed := withOpen(gatedSite(), libapi.KindUpload)
	seed.Status.LastAttempt.JobCreated = true
	h := newZAHarness(t, seed, crawlJobWith(seed, batchv1.JobFailed), crawlPod(seed, exited(0, ""), exited(2, "upload gave up")))
	st := h.attemptIn(seed, libapi.StateRegistered)
	st.Code, st.Error = libapi.CodeInsufficientSpace, "1 bytes free, 9 needed plus the reserve"

	h.reconcile(t, "site")

	za := assertFinished(t, h, "site", v1alpha1.AttemptResultFailed, v1alpha1.ReasonInsufficientSpace)
	assert.Equal(t, st.Error, za.Status.LastAttempt.Message)
	assertArchiveCondition(t, za, v1alpha1.ConditionDegraded, metav1.ConditionTrue, v1alpha1.ReasonInsufficientSpace)
}

func TestSourceKindChangedMidAttemptFails(t *testing.T) {
	flippedToDownload := withOpen(gatedSite(), libapi.KindUpload)
	flippedToDownload.Status.LastAttempt.JobCreated = true
	flippedToDownload.Spec.Source = v1alpha1.SourceSpec{Download: &v1alpha1.DownloadSource{URL: "https://example.org/site_2026-10.zim"}}
	flippedToCrawl := withOpen(urlArchive(), libapi.KindFetch)
	flippedToCrawl.Name = "site"
	flippedToCrawl.Spec.Source = testCrawlArchive().Spec.Source
	tests := []struct {
		name   string
		seed   *v1alpha1.ZimArchive
		reason string
	}{
		{name: "crawl to download", seed: flippedToDownload, reason: v1alpha1.ReasonUploadFailed},
		{name: "download to crawl", seed: flippedToCrawl, reason: v1alpha1.ReasonDownloadFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newZAHarness(t, tt.seed)
			h.attemptIn(tt.seed, libapi.StateReceiving)

			h.reconcile(t, "site")

			assert.Equal(t, []string{testAttempt}, h.librarian.discarded)
			za := assertFinished(t, h, "site", v1alpha1.AttemptResultFailed, tt.reason)
			assert.Equal(t, "source changed during the attempt", za.Status.LastAttempt.Message)
			assert.Empty(t, h.jobs(t), "no Job is built from the edited spec")
		})
	}
}
