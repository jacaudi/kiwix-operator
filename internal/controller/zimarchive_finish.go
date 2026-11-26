package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"slices"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/jacaudi/kiwix-operator/api/v1alpha1"
	"github.com/jacaudi/kiwix-operator/internal/gate"
	"github.com/jacaudi/kiwix-operator/internal/libapi"
	"github.com/jacaudi/kiwix-operator/internal/libclient"
	"github.com/jacaudi/kiwix-operator/internal/notify"
	"github.com/jacaudi/kiwix-operator/internal/schedule"
	"github.com/jacaudi/kiwix-operator/internal/source"
	"github.com/jacaudi/kiwix-operator/internal/zim"
)

// unreachableGrace is how long past its deadline an open attempt waits for an unreachable librarian.
const unreachableGrace = time.Hour

// heldDegraded are the Degraded reasons that hold until their cause is fixed, ahead of the last attempt's outcome.
var heldDegraded = []string{
	v1alpha1.ReasonInvalidSchedule, v1alpha1.ReasonAdoptFileInvalid, v1alpha1.ReasonAdoptConflict,
	v1alpha1.ReasonAdoptFileMissing, v1alpha1.ReasonCatalogLookupFailed,
}

// outcome is how an attempt ended; book is the current book after a success.
type outcome struct {
	result      v1alpha1.AttemptResult
	reason, msg string
	book        *libapi.BookInfo
}

func failed(reason, msg string) outcome {
	return outcome{result: v1alpha1.AttemptResultFailed, reason: reason, msg: msg}
}

// progress tracks the open attempt by its state in the librarian, which is authoritative, and finishes it once it has ended.
func (r *ZimArchiveReconciler) progress(ctx context.Context, za *v1alpha1.ZimArchive, lib *v1alpha1.KiwixLibrary) (ctrl.Result, error) {
	before := za.Status.DeepCopy()
	r.skipTicks(za)
	if lib == nil {
		return r.libraryGone(ctx, za, nil, before, fmt.Sprintf("KiwixLibrary %s is absent or being deleted", za.Spec.LibraryRef.Name))
	}
	lc, err := r.librarian(ctx, lib)
	if apierrors.IsNotFound(err) {
		return r.libraryGone(ctx, za, lib, before, err.Error())
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	// The Job is read before the attempt: a Job that ended after its upload staged the ZIM then shows as staged, not as a failure.
	job, err := r.crawlJob(ctx, za)
	if err != nil {
		return ctrl.Result{}, err
	}
	a := za.Status.LastAttempt
	st, err := lc.Get(ctx, a.ID)
	switch {
	case errors.Is(err, libclient.ErrNotFound):
		return r.reRegister(ctx, za, lib, lc, before)
	case err != nil:
		setArchiveCondition(za, v1alpha1.ConditionAvailable, metav1.ConditionFalse, v1alpha1.ReasonLibraryUnavailable, err.Error())
		if r.now().After(a.Deadline.Add(unreachableGrace)) {
			return r.finish(ctx, za, lib, before, failed(v1alpha1.ReasonLibraryUnavailable, "the librarian was unreachable past the attempt's deadline: "+err.Error()))
		}
		return ctrl.Result{RequeueAfter: statusPoll}, r.saveStatus(ctx, za, before)
	}
	setAvailable(za)
	return r.track(ctx, za, lib, lc, st, job, before)
}

// libraryGone finishes the open attempt Failed without calling the librarian, which its library no longer provides.
func (r *ZimArchiveReconciler) libraryGone(ctx context.Context, za *v1alpha1.ZimArchive, lib *v1alpha1.KiwixLibrary, before *v1alpha1.ZimArchiveStatus, msg string) (ctrl.Result, error) {
	setArchiveCondition(za, v1alpha1.ConditionAvailable, metav1.ConditionFalse, v1alpha1.ReasonLibraryUnavailable, msg)
	return r.finish(ctx, za, lib, before, failed(v1alpha1.ReasonLibraryUnavailable, msg))
}

// reRegister resumes creating an attempt the librarian does not know, unless its deadline has passed.
func (r *ZimArchiveReconciler) reRegister(ctx context.Context, za *v1alpha1.ZimArchive, lib *v1alpha1.KiwixLibrary, lc ArchiveLibrarian, before *v1alpha1.ZimArchiveStatus) (ctrl.Result, error) {
	a := za.Status.LastAttempt
	if !r.now().Before(a.Deadline.Time) {
		return r.finish(ctx, za, lib, before, failed(kindFailure(a.Kind), "the librarian does not know the attempt, and its deadline has passed"))
	}
	if err := r.ensureAttempt(ctx, za, lib, lc, nil, before); err != nil {
		return ctrl.Result{}, err
	}
	return r.poll(ctx, za, before)
}

// track acts on the open attempt's state in the librarian.
func (r *ZimArchiveReconciler) track(ctx context.Context, za *v1alpha1.ZimArchive, lib *v1alpha1.KiwixLibrary, lc ArchiveLibrarian, st libapi.AttemptState, job *batchv1.Job, before *v1alpha1.ZimArchiveStatus) (ctrl.Result, error) {
	a := za.Status.LastAttempt
	expired := !r.now().Before(a.Deadline.Time)
	switch st.State {
	case libapi.StateRegistered, libapi.StateReceiving:
		if a.Kind == libapi.KindUpload {
			return r.trackCrawl(ctx, za, lib, lc, job, before)
		}
		if expired {
			return r.discardAndFinish(ctx, za, lib, lc, before, failed(v1alpha1.ReasonDownloadFailed, "the download did not finish by its deadline"))
		}
		setArchiveCondition(za, v1alpha1.ConditionProgressing, metav1.ConditionTrue, v1alpha1.ReasonDownloading, "attempt "+a.ID+" is downloading")
		return r.poll(ctx, za, before)
	case libapi.StateStaged:
		return r.gateAndPromote(ctx, za, lib, lc, st, before)
	case libapi.StatePromoting:
		if expired {
			return r.discardAndFinish(ctx, za, lib, lc, before, failed(kindFailure(a.Kind), "the promotion did not finish by the attempt's deadline"))
		}
		setArchiveCondition(za, v1alpha1.ConditionProgressing, metav1.ConditionTrue, v1alpha1.ReasonPromoting, "attempt "+a.ID+" is being promoted")
		return r.poll(ctx, za, before)
	case libapi.StatePromoted:
		// The promotion succeeded but its status write was lost.
		book, err := lc.Archive(ctx, za.Name)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("read archive %s from the librarian: %w", za.Name, err)
		}
		if book == nil {
			return ctrl.Result{}, fmt.Errorf("attempt %s is promoted, but the librarian has no current book for %s", a.ID, za.Name)
		}
		return r.finish(ctx, za, lib, before, outcome{result: v1alpha1.AttemptResultSucceeded, book: book})
	case libapi.StateDiscarded:
		// A result on the open attempt is the outcome this operator recorded before its own Discard.
		if a.Result != "" {
			return r.finish(ctx, za, lib, before, outcome{result: a.Result, reason: a.Reason, msg: a.Message})
		}
		return r.finish(ctx, za, lib, before, failed(kindFailure(a.Kind), "discarded by the librarian"))
	case libapi.StateFailed:
		return r.finish(ctx, za, lib, before, failed(failureReason(a.Kind, st.Code), st.Error))
	}
	return ctrl.Result{}, fmt.Errorf("attempt %s: unknown librarian state %q", a.ID, st.State)
}

// trackCrawl follows a crawl whose upload has not staged: it creates the Job, or ends the attempt once the Job has ended or is gone.
func (r *ZimArchiveReconciler) trackCrawl(ctx context.Context, za *v1alpha1.ZimArchive, lib *v1alpha1.KiwixLibrary, lc ArchiveLibrarian, job *batchv1.Job, before *v1alpha1.ZimArchiveStatus) (ctrl.Result, error) {
	a := za.Status.LastAttempt
	if a.JobCreated && job == nil {
		return r.discardAndFinish(ctx, za, lib, lc, before, failed(v1alpha1.ReasonUploadFailed, "crawl Job gone"))
	}
	var pod *corev1.Pod
	if a.JobCreated {
		var err error
		if pod, err = r.crawlPod(ctx, za); err != nil {
			return ctrl.Result{}, err
		}
		if end := jobEnd(job); end != nil {
			reason, msg := crawlFailure(end, pod)
			return r.discardAndFinish(ctx, za, lib, lc, before, failed(reason, msg))
		}
	}
	if err := r.ensureJob(ctx, za, lib, before); err != nil {
		return ctrl.Result{}, err
	}
	reason, doing := v1alpha1.ReasonCrawling, "crawling"
	if pod != nil && containerState(pod.Status.ContainerStatuses, "upload").Running != nil {
		reason, doing = v1alpha1.ReasonUploading, "uploading"
	}
	setArchiveCondition(za, v1alpha1.ConditionProgressing, metav1.ConditionTrue, reason, "attempt "+a.ID+" is "+doing)
	return r.poll(ctx, za, before)
}

// gateAndPromote applies the health gate to a staged attempt against the librarian's current book, then promotes or discards it; after ErrCurrentChanged it re-gates once.
func (r *ZimArchiveReconciler) gateAndPromote(ctx context.Context, za *v1alpha1.ZimArchive, lib *v1alpha1.KiwixLibrary, lc ArchiveLibrarian, st libapi.AttemptState, before *v1alpha1.ZimArchiveStatus) (ctrl.Result, error) {
	a := za.Status.LastAttempt
	setArchiveCondition(za, v1alpha1.ConditionProgressing, metav1.ConditionTrue, v1alpha1.ReasonPromoting, "attempt "+a.ID+" is staged")
	for range 2 {
		d := gate.Decide(gateInput(za, st))
		if !d.Promote {
			o := outcome{result: d.Result, msg: d.Message}
			if d.Result == v1alpha1.AttemptResultRejected {
				o.reason = v1alpha1.ReasonGateRejected
			}
			return r.discardAndFinish(ctx, za, lib, lc, before, o)
		}
		book, err := lc.Promote(ctx, a.ID, r.promoteRequest(za, st))
		switch {
		case err == nil:
			return r.finish(ctx, za, lib, before, outcome{result: v1alpha1.AttemptResultSucceeded, book: &book})
		case errors.Is(err, libclient.ErrDuplicateBook):
			return r.discardAndFinish(ctx, za, lib, lc, before, outcome{result: v1alpha1.AttemptResultRejected, reason: v1alpha1.ReasonDuplicateBook, msg: err.Error()})
		case !errors.Is(err, libclient.ErrCurrentChanged):
			// Promote is idempotent, so the attempt stays staged and the retry promotes it.
			return ctrl.Result{}, errors.Join(r.saveStatus(ctx, za, before), fmt.Errorf("promote attempt %s: %w", a.ID, err))
		}
		if st, err = lc.Get(ctx, a.ID); err != nil {
			return ctrl.Result{}, fmt.Errorf("read attempt %s from the librarian: %w", a.ID, err)
		}
		if st.State != libapi.StateStaged {
			break
		}
	}
	return r.poll(ctx, za, before)
}

func gateInput(za *v1alpha1.ZimArchive, st libapi.AttemptState) gate.Input {
	d := za.Spec.Source.Download
	in := gate.Input{
		MinArticleRatio:   za.Spec.Refresh.MinArticleRatio,
		MinSizeRatio:      za.Spec.Refresh.MinSizeRatio,
		URLWithoutSpecSHA: d != nil && d.Catalog == nil && d.SHA256 == "",
	}
	if s := st.Staged; s != nil {
		in.Staged, in.StagedSHA = gate.Counts{ArticleCount: s.ArticleCount, SizeKiB: s.SizeKiB}, s.SHA256
	}
	if c := st.Current; c != nil {
		in.Current, in.CurrentSHA = &gate.Counts{ArticleCount: c.ArticleCount, SizeKiB: c.SizeKiB}, c.SHA256
	}
	return in
}

// promoteRequest promotes over the current book the attempt was gated against.
func (r *ZimArchiveReconciler) promoteRequest(za *v1alpha1.ZimArchive, st libapi.AttemptState) libapi.PromoteRequest {
	req := libapi.PromoteRequest{SourceVersion: promoteSourceVersion(za, r.now()), SourceFingerprint: source.Fingerprint(za.Spec.Source)}
	if st.Current != nil {
		req.ExpectedCurrentBookID = st.Current.BookID
	}
	return req
}

// promoteSourceVersion is the YYYY-MM version a promotion records: the catalog version the attempt fetched, the version in a URL's file name, else the promotion month.
func promoteSourceVersion(za *v1alpha1.ZimArchive, now time.Time) string {
	d := za.Spec.Source.Download
	switch {
	case d == nil:
	case d.Catalog != nil:
		if v := za.Status.LastAttempt.SourceVersion; v != "" {
			return v
		}
	default:
		if u, err := url.Parse(d.URL); err == nil {
			if v, ok := zim.NormalizeVersion(path.Base(u.Path)); ok {
				return v
			}
		}
	}
	return now.UTC().Format("2006-01")
}

// discardAndFinish records o on the still-open attempt, discards the attempt in the librarian, then finishes it with o; a final refusal, such as a promotion in progress, still finishes it.
func (r *ZimArchiveReconciler) discardAndFinish(ctx context.Context, za *v1alpha1.ZimArchive, lib *v1alpha1.KiwixLibrary, lc ArchiveLibrarian, before *v1alpha1.ZimArchiveStatus, o outcome) (ctrl.Result, error) {
	a := za.Status.LastAttempt
	id := a.ID
	// Discard is irreversible, so the outcome is written first: if the finishing write is lost, the discarded row finishes with it.
	a.Result, a.Reason, a.Message = o.result, o.reason, o.msg
	if err := r.saveStatus(ctx, za, before); err != nil {
		return ctrl.Result{}, err
	}
	if _, err := lc.Discard(ctx, id); err != nil {
		if libclient.Retryable(err) {
			return ctrl.Result{}, fmt.Errorf("discard attempt %s: %w", id, err)
		}
		ctrl.LoggerFrom(ctx).Info("librarian refused the discard; finishing the attempt anyway", "attempt", id, "error", err.Error())
	}
	return r.finish(ctx, za, lib, before, o)
}

// finish closes the open attempt with o, deletes its upload Secret, saves the status, then sends the attempt's notification.
func (r *ZimArchiveReconciler) finish(ctx context.Context, za *v1alpha1.ZimArchive, lib *v1alpha1.KiwixLibrary, before *v1alpha1.ZimArchiveStatus, o outcome) (ctrl.Result, error) {
	now := r.now()
	a := za.Status.LastAttempt
	id := a.ID
	if a.Kind == libapi.KindUpload {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: za.Namespace, Name: uploadSecretName(za.Name, id)}}
		if err := r.Delete(ctx, s); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, fmt.Errorf("delete upload Secret %s: %w", s.Name, err)
		}
	}
	a.FinishedAt = new(metav1.NewTime(now))
	a.Result, a.Reason, a.Message = o.result, o.reason, o.msg
	switch o.result {
	case v1alpha1.AttemptResultSucceeded:
		var prevArticles int64
		prev := za.Status.Current
		if prev != nil {
			prevArticles = prev.ArticleCount
		}
		// The message is the success notification's, which needs the previous version.
		_, a.Message = notify.Updated(za.Name, o.book.ArticleCount, prevArticles, o.book.SizeKiB, prev == nil)
		za.Status.Current = currentFrom(*o.book)
		za.Status.Current.PromotedAt = new(metav1.NewTime(now))
		za.Status.FailedAttempts, za.Status.LastFailureNotifiedAt = 0, nil
		setAvailable(za)
	case v1alpha1.AttemptResultUnchanged:
		a.ID, a.StartedAt = "", *a.FinishedAt
	default:
		if za.Status.Current == nil {
			za.Status.FailedAttempts++
		}
	}
	r.skipTicks(za)
	if a.Trigger == schedule.TriggerRequested {
		za.Status.LastHandledRequestedAt = a.RequestedAt
	}
	settleDegraded(za)
	setIdle(za)
	a.Notified = !r.notificationDue(za, lib)
	next := r.nextRefresh(za, now)
	if err := r.saveStatus(ctx, za, before); err != nil {
		return ctrl.Result{}, err
	}
	eventType := corev1.EventTypeWarning
	if o.result == v1alpha1.AttemptResultSucceeded || o.result == v1alpha1.AttemptResultUnchanged {
		eventType = corev1.EventTypeNormal
	}
	r.Recorder.Eventf(za, nil, eventType, attemptEvent(o.result), "Finish", "%s attempt %s: %s", a.Trigger, id, a.Message)
	return ctrl.Result{RequeueAfter: max(refreshAfter(next, now), time.Second)}, r.notifyAttempt(ctx, za, lib)
}

// attemptEvent is the Event reason of an attempt that ended with result.
func attemptEvent(result v1alpha1.AttemptResult) string { return "Attempt" + string(result) }

// nextRefresh sets nextRefreshAt for the archive whose attempt just finished, now when a trigger is already pending, and returns it.
func (r *ZimArchiveReconciler) nextRefresh(za *v1alpha1.ZimArchive, now time.Time) time.Time {
	due, err := schedule.Evaluate(dueInput(za), now)
	switch {
	case err != nil:
		// Degraded InvalidSchedule reports it; only a spec change can fix it.
		due.Next = time.Time{}
	case due.Due:
		due.Next = now
	}
	za.Status.NextRefreshAt = optTime(due.Next)
	return due.Next
}

// skipTicks records a schedule tick that passed while the attempt ran as skipped, so it starts no attempt once this one ends; an invalid schedule sets Degraded instead.
func (r *ZimArchiveReconciler) skipTicks(za *v1alpha1.ZimArchive) {
	in := dueInput(za)
	in.Running = true
	due, err := schedule.Evaluate(in, r.now())
	if err != nil {
		setArchiveCondition(za, v1alpha1.ConditionDegraded, metav1.ConditionTrue, v1alpha1.ReasonInvalidSchedule, err.Error())
		return
	}
	clearDegraded(za, v1alpha1.ReasonInvalidSchedule)
	if due.SkippedTick.IsZero() {
		return
	}
	za.Status.LastScheduledAt = new(metav1.NewTime(due.SkippedTick))
	r.Recorder.Eventf(za, nil, corev1.EventTypeWarning, "ScheduleSkipped", "Schedule", "the %s tick passed while attempt %s ran",
		due.SkippedTick.Format(time.RFC3339), za.Status.LastAttempt.ID)
}

// notificationDue reports whether za's finished attempt notifies: a success only with notify.onSuccess, a failure or rejection within the failure rate limit, Unchanged never.
func (r *ZimArchiveReconciler) notificationDue(za *v1alpha1.ZimArchive, lib *v1alpha1.KiwixLibrary) bool {
	if lib == nil || lib.Spec.Notify == nil {
		return false
	}
	switch za.Status.LastAttempt.Result {
	case v1alpha1.AttemptResultSucceeded:
		return za.Spec.Notify.OnSuccess
	case v1alpha1.AttemptResultFailed, v1alpha1.AttemptResultRejected:
		var last *time.Time
		if t := za.Status.LastFailureNotifiedAt; t != nil {
			last = &t.Time
		}
		return schedule.ShouldNotifyFailure(za.Status.FailedAttempts, last, r.now())
	}
	return false
}

// notifyAttempt sends the notification of za's finished attempt once. The status write that records it follows the send, so a lost write repeats the notification rather than losing it.
func (r *ZimArchiveReconciler) notifyAttempt(ctx context.Context, za *v1alpha1.ZimArchive, lib *v1alpha1.KiwixLibrary) error {
	a := za.Status.LastAttempt
	if a == nil || a.FinishedAt == nil || a.Notified || !r.notificationDue(za, lib) {
		return nil
	}
	before := za.Status.DeepCopy()
	title, msg := attemptNotification(za)
	if r.notify(ctx, za, lib, title, msg) && a.Result != v1alpha1.AttemptResultSucceeded {
		za.Status.LastFailureNotifiedAt = new(metav1.NewTime(r.now()))
	}
	a.Notified = true
	return r.saveStatus(ctx, za, before)
}

// attemptNotification is the notification of za's finished attempt, built from its status alone so a resend after a lost write matches.
func attemptNotification(za *v1alpha1.ZimArchive) (title, msg string) {
	a := za.Status.LastAttempt
	switch a.Result {
	case v1alpha1.AttemptResultSucceeded:
		// finish rendered the message into the attempt; only the title is taken here.
		title, _ = notify.Updated(za.Name, 0, 0, 0, true)
		return title, a.Message
	case v1alpha1.AttemptResultRejected:
		return notify.Rejected(za.Name, a.Message)
	}
	return notify.Failed(za.Name, a.Reason+": "+a.Message)
}

// kindFailure is the reason of an attempt of kind that failed with no more specific cause.
func kindFailure(kind string) string {
	if kind == libapi.KindUpload {
		return v1alpha1.ReasonUploadFailed
	}
	return v1alpha1.ReasonDownloadFailed
}

// failureReason is the reason of an attempt the librarian failed with code.
func failureReason(kind, code string) string {
	switch code {
	case libapi.CodeHashMismatch:
		return v1alpha1.ReasonHashMismatch
	case libapi.CodeInsufficientSpace:
		return v1alpha1.ReasonInsufficientSpace
	}
	return kindFailure(kind)
}

// crawlJob returns the open crawl's Job, or nil when it was never created or is gone.
func (r *ZimArchiveReconciler) crawlJob(ctx context.Context, za *v1alpha1.ZimArchive) (*batchv1.Job, error) {
	a := za.Status.LastAttempt
	if a.Kind != libapi.KindUpload || !a.JobCreated {
		return nil, nil
	}
	var job batchv1.Job
	key := client.ObjectKey{Namespace: za.Namespace, Name: jobName(za.Name, a.ID)}
	err := r.Get(ctx, key, &job)
	if apierrors.IsNotFound(err) {
		// A missing Job discards the attempt, and the cache may not have a Job ensureJob has just created, so the API server confirms it.
		err = r.apiReader.Get(ctx, key, &job)
	}
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get crawl Job of attempt %s: %w", a.ID, err)
	}
	return &job, nil
}

// crawlPod returns the open crawl's pod, or nil before it exists.
func (r *ZimArchiveReconciler) crawlPod(ctx context.Context, za *v1alpha1.ZimArchive) (*corev1.Pod, error) {
	var pods corev1.PodList
	attempt := za.Status.LastAttempt.ID
	if err := r.List(ctx, &pods, client.InNamespace(za.Namespace),
		client.MatchingLabels{v1alpha1.LabelArchive: za.Name, v1alpha1.LabelAttempt: attempt}); err != nil {
		return nil, fmt.Errorf("list crawl pods of attempt %s: %w", attempt, err)
	}
	if len(pods.Items) == 0 {
		return nil, nil
	}
	// ponytail: the Job's backoffLimit is 0, so it has one pod; pick the newest if that ever changes.
	return &pods.Items[0], nil
}

// jobEnd is the condition that ended job, nil while it runs.
func jobEnd(job *batchv1.Job) *batchv1.JobCondition {
	i := slices.IndexFunc(job.Status.Conditions, func(c batchv1.JobCondition) bool {
		return (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue
	})
	if i < 0 {
		return nil
	}
	return &job.Status.Conditions[i]
}

// crawlFailure is the reason and message of a crawl Job that ended without staging its ZIM: CrawlFailed when zimit failed, else UploadFailed, with the failing container's termination message.
func crawlFailure(end *batchv1.JobCondition, pod *corev1.Pod) (reason, msg string) {
	if pod != nil {
		if t := containerState(pod.Status.InitContainerStatuses, "zimit").Terminated; t != nil && t.ExitCode != 0 {
			return v1alpha1.ReasonCrawlFailed, cmp.Or(t.Message, fmt.Sprintf("zimit exited with code %d", t.ExitCode))
		}
		if t := containerState(pod.Status.ContainerStatuses, "upload").Terminated; t != nil && t.ExitCode != 0 {
			return v1alpha1.ReasonUploadFailed, cmp.Or(t.Message, fmt.Sprintf("upload exited with code %d", t.ExitCode))
		}
	}
	return v1alpha1.ReasonUploadFailed, cmp.Or(end.Message, "crawl Job ended without staging the ZIM")
}

func containerState(statuses []corev1.ContainerStatus, name string) corev1.ContainerState {
	i := slices.IndexFunc(statuses, func(s corev1.ContainerStatus) bool { return s.Name == name })
	if i < 0 {
		return corev1.ContainerState{}
	}
	return statuses[i].State
}

// poll saves the open attempt's status and looks at it again after openAttemptPoll.
func (r *ZimArchiveReconciler) poll(ctx context.Context, za *v1alpha1.ZimArchive, before *v1alpha1.ZimArchiveStatus) (ctrl.Result, error) {
	return ctrl.Result{RequeueAfter: openAttemptPoll}, r.saveStatus(ctx, za, before)
}

// settleDegraded sets Degraded from the last finished attempt unless a held reason is in force.
func settleDegraded(za *v1alpha1.ZimArchive) {
	if c := meta.FindStatusCondition(za.Status.Conditions, v1alpha1.ConditionDegraded); c != nil &&
		c.Status == metav1.ConditionTrue && slices.Contains(heldDegraded, c.Reason) {
		return
	}
	attemptDegraded(za)
}

// attemptDegraded sets Degraded to the last finished attempt's reason when it failed or was rejected, else Healthy.
func attemptDegraded(za *v1alpha1.ZimArchive) {
	if a := za.Status.LastAttempt; a != nil && a.FinishedAt != nil &&
		(a.Result == v1alpha1.AttemptResultFailed || a.Result == v1alpha1.AttemptResultRejected) {
		setArchiveCondition(za, v1alpha1.ConditionDegraded, metav1.ConditionTrue, a.Reason, a.Message)
		return
	}
	setArchiveCondition(za, v1alpha1.ConditionDegraded, metav1.ConditionFalse, v1alpha1.ReasonHealthy, "no problem found")
}

// defaultConditions sets each condition a reconcile left unset, so all three are always present, and marks every condition observed at the current generation.
func defaultConditions(za *v1alpha1.ZimArchive) {
	for i := range za.Status.Conditions {
		za.Status.Conditions[i].ObservedGeneration = za.Generation
	}
	if meta.FindStatusCondition(za.Status.Conditions, v1alpha1.ConditionAvailable) == nil {
		setAvailable(za)
	}
	if meta.FindStatusCondition(za.Status.Conditions, v1alpha1.ConditionProgressing) == nil {
		setIdle(za)
	}
	if meta.FindStatusCondition(za.Status.Conditions, v1alpha1.ConditionDegraded) == nil {
		attemptDegraded(za)
	}
}
