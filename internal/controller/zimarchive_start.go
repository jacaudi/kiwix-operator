package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/jacaudi/kiwix-operator/api/v1alpha1"
	"github.com/jacaudi/kiwix-operator/internal/catalog"
	"github.com/jacaudi/kiwix-operator/internal/gate"
	"github.com/jacaudi/kiwix-operator/internal/libapi"
	"github.com/jacaudi/kiwix-operator/internal/libclient"
	"github.com/jacaudi/kiwix-operator/internal/notify"
	"github.com/jacaudi/kiwix-operator/internal/schedule"
	"github.com/jacaudi/kiwix-operator/internal/source"
)

// fetchDeadline is how long the librarian keeps a fetch attempt before discarding it.
const fetchDeadline = 24 * time.Hour

// start mirrors the librarian's current book, adopts spec.adoptFile, and starts an attempt when one is due.
func (r *ZimArchiveReconciler) start(ctx context.Context, za *v1alpha1.ZimArchive, lib *v1alpha1.KiwixLibrary) (ctrl.Result, error) {
	before := za.Status.DeepCopy()
	lc, err := r.librarian(ctx, lib)
	if err == nil {
		err = r.mirror(ctx, za, lc)
	}
	if err != nil {
		setArchiveCondition(za, v1alpha1.ConditionAvailable, metav1.ConditionFalse, v1alpha1.ReasonLibraryUnavailable, err.Error())
		return ctrl.Result{RequeueAfter: statusPoll}, r.saveStatus(ctx, za, before)
	}

	if za.Spec.AdoptFile != "" && za.Status.Current == nil {
		reason, err := r.adopt(ctx, za, lc)
		if err != nil {
			return ctrl.Result{}, err
		}
		if reason != "" {
			return r.awaitAdoption(ctx, za, lib, reason, before)
		}
		// The status write re-triggers the reconcile, which evaluates due against the adopted version.
		return ctrl.Result{}, r.saveStatus(ctx, za, before)
	}

	due, err := schedule.Evaluate(dueInput(za), r.now())
	if err != nil {
		setArchiveCondition(za, v1alpha1.ConditionDegraded, metav1.ConditionTrue, v1alpha1.ReasonInvalidSchedule, err.Error())
		setIdle(za)
		// No requeue: only a spec change can fix the schedule, and it triggers a reconcile.
		return ctrl.Result{}, r.saveStatus(ctx, za, before)
	}
	clearDegraded(za, v1alpha1.ReasonInvalidSchedule, v1alpha1.ReasonAdoptFileMissing, v1alpha1.ReasonAdoptFileInvalid, v1alpha1.ReasonAdoptConflict)
	if !due.Due {
		setIdle(za)
		za.Status.NextRefreshAt = optTime(due.Next)
		return ctrl.Result{RequeueAfter: refreshAfter(due.Next, r.now())}, r.saveStatus(ctx, za, before)
	}
	return r.startDue(ctx, za, lib, lc, due, before)
}

// startDue runs the pre-check and the concurrency limit for a due archive, then creates the attempt.
func (r *ZimArchiveReconciler) startDue(ctx context.Context, za *v1alpha1.ZimArchive, lib *v1alpha1.KiwixLibrary, lc ArchiveLibrarian, due schedule.Due, before *v1alpha1.ZimArchiveStatus) (ctrl.Result, error) {
	entry, unchanged, err := r.preCheck(ctx, za)
	if err != nil {
		setArchiveCondition(za, v1alpha1.ConditionDegraded, metav1.ConditionTrue, v1alpha1.ReasonCatalogLookupFailed, err.Error())
		setIdle(za)
		return ctrl.Result{RequeueAfter: catalogRetry}, r.saveStatus(ctx, za, before)
	}
	clearDegraded(za, v1alpha1.ReasonCatalogLookupFailed)
	if unchanged {
		r.recordUnchanged(za, due)
		return ctrl.Result{}, r.saveStatus(ctx, za, before)
	}

	kind := attemptKind(za)
	n, err := r.openAttempts(ctx, za, kind)
	if err != nil {
		return ctrl.Result{}, err
	}
	if limit := concurrencyLimit(lib, kind); n >= int(limit) {
		setArchiveCondition(za, v1alpha1.ConditionProgressing, metav1.ConditionTrue, v1alpha1.ReasonQueued,
			fmt.Sprintf("%d of %d %s attempts already running in KiwixLibrary %s", n, limit, kind, lib.Name))
		return ctrl.Result{RequeueAfter: queuedRetry}, r.saveStatus(ctx, za, before)
	}
	return r.createAttempt(ctx, za, lib, lc, due, entry, before)
}

// mirror copies the librarian's current book into status when its book ID differs; a lost book clears current and lastAttempt so the archive is due as initial.
func (r *ZimArchiveReconciler) mirror(ctx context.Context, za *v1alpha1.ZimArchive, lc ArchiveLibrarian) error {
	book, err := lc.Archive(ctx, za.Name)
	if err != nil {
		return fmt.Errorf("read archive %s from the librarian: %w", za.Name, err)
	}
	cur := za.Status.Current
	switch {
	case book == nil && cur != nil:
		r.Recorder.Eventf(za, nil, corev1.EventTypeWarning, v1alpha1.ReasonCurrentLost, "Mirror", "%s is no longer in the library", cur.File)
		za.Status.Current, za.Status.LastAttempt = nil, nil
	case book != nil && (cur == nil || cur.BookID != book.BookID):
		// An inherited book was not promoted by this archive, so it has no promotedAt.
		za.Status.Current = currentFrom(*book)
	}
	if za.Status.Current == nil {
		setArchiveCondition(za, v1alpha1.ConditionAvailable, metav1.ConditionFalse, v1alpha1.ReasonNotYetFetched, "no version in the library yet")
		return nil
	}
	setArchiveCondition(za, v1alpha1.ConditionAvailable, metav1.ConditionTrue, v1alpha1.ReasonServed, "serving "+za.Status.Current.File)
	return nil
}

func currentFrom(b libapi.BookInfo) *v1alpha1.CurrentVersion {
	return &v1alpha1.CurrentVersion{
		File: b.File, BookID: b.BookID, ArticleCount: b.ArticleCount, SizeKiB: b.SizeKiB, SHA256: b.SHA256,
		SourceVersion: b.SourceVersion, SourceFingerprint: b.SourceFingerprint,
	}
}

// adopt makes spec.adoptFile the archive's current book. A non-empty reason is the Degraded reason the archive waits under instead of fetching.
func (r *ZimArchiveReconciler) adopt(ctx context.Context, za *v1alpha1.ZimArchive, lc ArchiveLibrarian) (reason string, err error) {
	file := za.Spec.AdoptFile
	winner, err := r.adoptWinner(ctx, za)
	if err != nil {
		return "", err
	}
	var msg string
	if winner != za.Name {
		reason, msg = v1alpha1.ReasonAdoptConflict, fmt.Sprintf("ZimArchive %s, created first, also adopts %s", winner, file)
	} else {
		book, err := lc.Adopt(ctx, za.Name, libapi.AdoptRequest{File: file})
		switch {
		case errors.Is(err, libclient.ErrAdoptMissing):
			reason, msg = v1alpha1.ReasonAdoptFileMissing, file+" is not on the library volume; remove adoptFile to fetch instead"
		case errors.Is(err, libclient.ErrAdoptInvalid):
			reason, msg = v1alpha1.ReasonAdoptFileInvalid, err.Error()
		case err != nil:
			return "", fmt.Errorf("adopt %s as %s: %w", file, za.Name, err)
		default:
			za.Status.Current = currentFrom(book)
			za.Status.Current.PromotedAt = new(metav1.NewTime(r.now()))
			setArchiveCondition(za, v1alpha1.ConditionAvailable, metav1.ConditionTrue, v1alpha1.ReasonServed, "serving "+book.File)
			r.Recorder.Eventf(za, nil, corev1.EventTypeNormal, "Adopted", "Adopt", "adopted %s as %s", file, book.File)
			return "", nil
		}
	}
	setArchiveCondition(za, v1alpha1.ConditionDegraded, metav1.ConditionTrue, reason, msg)
	setArchiveCondition(za, v1alpha1.ConditionAvailable, metav1.ConditionFalse, v1alpha1.ReasonAwaitingAdoption, "waiting to adopt "+file)
	return reason, nil
}

// adoptWinner is the archive that may adopt za's adoptFile: of the archives of za's library still waiting to adopt it, the one created first, then the lowest name.
func (r *ZimArchiveReconciler) adoptWinner(ctx context.Context, za *v1alpha1.ZimArchive) (string, error) {
	var list v1alpha1.ZimArchiveList
	if err := r.List(ctx, &list, client.InNamespace(za.Namespace), client.MatchingFields{libraryRefField: za.Spec.LibraryRef.Name}); err != nil {
		return "", fmt.Errorf("list ZimArchives of KiwixLibrary %s: %w", za.Spec.LibraryRef.Name, err)
	}
	winner := za
	for i := range list.Items {
		o := &list.Items[i]
		if o.Spec.AdoptFile != za.Spec.AdoptFile || o.Status.Current != nil {
			continue
		}
		if o.CreationTimestamp.Before(&winner.CreationTimestamp) ||
			(o.CreationTimestamp.Equal(&winner.CreationTimestamp) && o.Name < winner.Name) {
			winner = o
		}
	}
	return winner.Name, nil
}

// awaitAdoption saves the waiting status, then sends the missing-file notification once; a notification never blocks the status write.
func (r *ZimArchiveReconciler) awaitAdoption(ctx context.Context, za *v1alpha1.ZimArchive, lib *v1alpha1.KiwixLibrary, reason string, before *v1alpha1.ZimArchiveStatus) (ctrl.Result, error) {
	send := reason == v1alpha1.ReasonAdoptFileMissing && !za.Status.AdoptMissingNotified
	if send {
		za.Status.AdoptMissingNotified = true
	}
	setIdle(za)
	if err := r.saveStatus(ctx, za, before); err != nil {
		return ctrl.Result{}, err
	}
	if send {
		title, msg := notify.AdoptMissing(za.Name, za.Spec.AdoptFile)
		r.notify(ctx, za, lib, title, msg)
	}
	// The file may appear or the winner may finish, neither of which is an event this controller watches.
	return ctrl.Result{RequeueAfter: statusPoll}, nil
}

// notify posts to lib's webhook when it has one; a failure is logged and recorded as an Event, never returned.
func (r *ZimArchiveReconciler) notify(ctx context.Context, za *v1alpha1.ZimArchive, lib *v1alpha1.KiwixLibrary, title, msg string) {
	if lib.Spec.Notify == nil {
		return
	}
	if err := r.Notifier.Send(ctx, lib.Spec.Notify.Webhook.URL, title, msg); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "notification failed", "title", title)
		r.Recorder.Eventf(za, nil, corev1.EventTypeWarning, "NotifyFailed", "Notify", "%s: %v", title, err)
	}
}

func dueInput(za *v1alpha1.ZimArchive) schedule.DueInput {
	in := schedule.DueInput{
		HasCurrent:             za.Status.Current != nil,
		FailedAttempts:         za.Status.FailedAttempts,
		CreatedAt:              za.CreationTimestamp.Time,
		Schedule:               za.Spec.Refresh.Schedule,
		TimeZone:               za.Spec.Refresh.TimeZone,
		RequestedAt:            za.Annotations[v1alpha1.AnnotationRequestedAt],
		LastHandledRequestedAt: za.Status.LastHandledRequestedAt,
	}
	if t := za.Status.LastScheduledAt; t != nil {
		in.LastScheduledAt = t.Time
	}
	if a := za.Status.LastAttempt; a != nil {
		in.EverAttempted = true
		in.LastFailedOrRejected = a.Result == v1alpha1.AttemptResultFailed || a.Result == v1alpha1.AttemptResultRejected
		if a.FinishedAt != nil {
			in.LastFinished = a.FinishedAt.Time
		}
	}
	return in
}

// preCheck looks up the catalog entry for a catalog source and reports whether the source has nothing newer than current.
func (r *ZimArchiveReconciler) preCheck(ctx context.Context, za *v1alpha1.ZimArchive) (*catalog.Entry, bool, error) {
	src := za.Spec.Source
	in := gate.PreCheckInput{SpecFingerprint: source.Fingerprint(src), Kind: gate.KindCrawl}
	if cur := za.Status.Current; cur != nil {
		in.HasCurrent = true
		in.CurrentFingerprint, in.CurrentVersion, in.CurrentSHA = cur.SourceFingerprint, cur.SourceVersion, cur.SHA256
	}
	var entry *catalog.Entry
	switch {
	case src.Download == nil:
	case src.Download.Catalog != nil:
		e, err := r.Catalog.Latest(ctx, src.Download.Catalog.Name, src.Download.Catalog.Flavour)
		if err != nil {
			return nil, false, err
		}
		entry = &e
		in.Kind, in.CandidateVersion = gate.KindCatalog, e.Version
	default:
		in.Kind, in.SpecSHA = gate.KindURL, src.Download.SHA256
	}
	return entry, gate.Unchanged(in), nil
}

// recordUnchanged closes the due trigger with an Unchanged result that is never open.
func (r *ZimArchiveReconciler) recordUnchanged(za *v1alpha1.ZimArchive, due schedule.Due) {
	now := metav1.NewTime(r.now())
	za.Status.LastAttempt = &v1alpha1.AttemptStatus{
		Trigger:     due.Trigger,
		RequestedAt: requestedBy(za, due),
		StartedAt:   now,
		FinishedAt:  new(now),
		Result:      v1alpha1.AttemptResultUnchanged,
		Message:     "the source has nothing newer than the current version",
	}
	consumeTrigger(za, due)
	setIdle(za)
	r.Recorder.Eventf(za, nil, corev1.EventTypeNormal, string(v1alpha1.AttemptResultUnchanged), "Refresh", "%s refresh: %s", due.Trigger, za.Status.LastAttempt.Message)
}

// requestedBy is the requestedAt value that triggered due, or "" for another trigger.
func requestedBy(za *v1alpha1.ZimArchive, due schedule.Due) string {
	if due.Trigger != schedule.TriggerRequested {
		return ""
	}
	return za.Annotations[v1alpha1.AnnotationRequestedAt]
}

// consumeTrigger records the tick and the requestedAt value due acted on, so neither fires again.
func consumeTrigger(za *v1alpha1.ZimArchive, due schedule.Due) {
	if !due.ScheduleTick.IsZero() {
		za.Status.LastScheduledAt = new(metav1.NewTime(due.ScheduleTick))
	}
	if due.Trigger == schedule.TriggerRequested {
		za.Status.LastHandledRequestedAt = za.Annotations[v1alpha1.AnnotationRequestedAt]
	}
}

// attemptKind is the librarian attempt kind of za's source.
func attemptKind(za *v1alpha1.ZimArchive) string {
	if za.Spec.Source.Crawl != nil {
		return libapi.KindUpload
	}
	return libapi.KindFetch
}

func concurrencyLimit(lib *v1alpha1.KiwixLibrary, kind string) int32 {
	if kind == libapi.KindUpload {
		return lib.Spec.Crawls.MaxConcurrent
	}
	return lib.Spec.Downloads.MaxConcurrent
}

// openAttempts counts the open attempts of kind in za's library, za's own excluded, read from the API server rather than the cache.
func (r *ZimArchiveReconciler) openAttempts(ctx context.Context, za *v1alpha1.ZimArchive, kind string) (int, error) {
	var list v1alpha1.ZimArchiveList
	if err := r.apiReader.List(ctx, &list, client.InNamespace(za.Namespace)); err != nil {
		return 0, fmt.Errorf("list ZimArchives in %s: %w", za.Namespace, err)
	}
	n := 0
	for _, o := range list.Items {
		if o.Name != za.Name && o.Spec.LibraryRef.Name == za.Spec.LibraryRef.Name &&
			attemptOpen(o.Status.LastAttempt) && o.Status.LastAttempt.Kind == kind {
			n++
		}
	}
	return n, nil
}

// createAttempt records the new attempt in status, consuming its trigger, then creates its objects.
func (r *ZimArchiveReconciler) createAttempt(ctx context.Context, za *v1alpha1.ZimArchive, lib *v1alpha1.KiwixLibrary, lc ArchiveLibrarian, due schedule.Due, entry *catalog.Entry, before *v1alpha1.ZimArchiveStatus) (ctrl.Result, error) {
	now := r.now()
	deadline, reason := now.Add(fetchDeadline), v1alpha1.ReasonDownloading
	if c := za.Spec.Source.Crawl; c != nil {
		deadline, reason = now.Add(crawlJobDeadline(c)+time.Hour), v1alpha1.ReasonCrawling
	}
	za.Status.LastAttempt = &v1alpha1.AttemptStatus{
		ID:           newAttemptID(),
		Kind:         attemptKind(za),
		Trigger:      due.Trigger,
		RequestedAt:  requestedBy(za, due),
		ScheduleTick: optTime(due.ScheduleTick),
		StartedAt:    metav1.NewTime(now),
		Deadline:     new(metav1.NewTime(deadline)),
	}
	consumeTrigger(za, due)
	setArchiveCondition(za, v1alpha1.ConditionProgressing, metav1.ConditionTrue, reason,
		fmt.Sprintf("attempt %s started (%s)", za.Status.LastAttempt.ID, due.Trigger))
	if err := r.saveStatus(ctx, za, before); err != nil {
		return ctrl.Result{}, err
	}
	return r.ensureAttempt(ctx, za, lib, lc, entry, before)
}

// resume finishes creating an open attempt whose start was interrupted.
func (r *ZimArchiveReconciler) resume(ctx context.Context, za *v1alpha1.ZimArchive, lib *v1alpha1.KiwixLibrary) (ctrl.Result, error) {
	lc, err := r.librarian(ctx, lib)
	if err != nil {
		return ctrl.Result{}, err
	}
	return r.ensureAttempt(ctx, za, lib, lc, nil, za.Status.DeepCopy())
}

// ensureAttempt creates, in order, the upload Secret, the registration and the crawl Job with its ConfigMap, skipping each that already exists.
func (r *ZimArchiveReconciler) ensureAttempt(ctx context.Context, za *v1alpha1.ZimArchive, lib *v1alpha1.KiwixLibrary, lc ArchiveLibrarian, entry *catalog.Entry, before *v1alpha1.ZimArchiveStatus) (ctrl.Result, error) {
	var tokenHash string
	if za.Spec.Source.Crawl != nil {
		token, err := r.ensureUploadSecret(ctx, za)
		if err != nil {
			return ctrl.Result{}, err
		}
		sum := sha256.Sum256([]byte(token))
		tokenHash = hex.EncodeToString(sum[:])
	}
	if err := r.ensureRegistered(ctx, za, lc, tokenHash, entry); err != nil {
		return ctrl.Result{}, err
	}
	if za.Spec.Source.Crawl != nil {
		if err := r.ensureJob(ctx, za, lib, before); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: openAttemptPoll}, nil
}

// ensureUploadSecret returns the attempt's upload token, creating its Secret, owned by the archive, when there is none.
func (r *ZimArchiveReconciler) ensureUploadSecret(ctx context.Context, za *v1alpha1.ZimArchive) (string, error) {
	attempt := za.Status.LastAttempt.ID
	var got corev1.Secret
	err := r.Get(ctx, client.ObjectKey{Namespace: za.Namespace, Name: uploadSecretName(za.Name, attempt)}, &got)
	if err == nil {
		return string(got.Data[uploadTokenKey]), nil
	}
	if !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("get upload Secret of attempt %s: %w", attempt, err)
	}
	s := buildUploadSecret(za, attempt, rand.Text())
	if err := controllerutil.SetControllerReference(za, s, r.Scheme); err != nil {
		return "", fmt.Errorf("own upload Secret %s: %w", s.Name, err)
	}
	// AlreadyExists is an error here: the token in the existing Secret is the one to register.
	if err := r.Create(ctx, s); err != nil {
		return "", fmt.Errorf("create upload Secret %s: %w", s.Name, err)
	}
	return string(s.Data[uploadTokenKey]), nil
}

// ensureRegistered registers the attempt unless the librarian already knows it; a catalog fetch resumed after a restart looks its entry up again.
func (r *ZimArchiveReconciler) ensureRegistered(ctx context.Context, za *v1alpha1.ZimArchive, lc ArchiveLibrarian, tokenHash string, entry *catalog.Entry) error {
	a := za.Status.LastAttempt
	_, err := lc.Get(ctx, a.ID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, libclient.ErrNotFound) {
		return fmt.Errorf("read attempt %s from the librarian: %w", a.ID, err)
	}
	req := libapi.RegisterRequest{
		Archive: za.Name, Attempt: a.ID, Kind: a.Kind, TokenHash: tokenHash, Deadline: a.Deadline.Time,
	}
	if cur := za.Status.Current; cur != nil {
		req.ExpectedCurrentBookID = cur.BookID
	}
	if c := za.Spec.Source.Crawl; c != nil {
		req.Parts = []string{libapi.PartZIM}
		if c.KeepWarcs {
			req.Parts = append(req.Parts, libapi.PartWARCs)
		}
	} else if req.Fetch, err = r.fetchSpec(ctx, za.Spec.Source.Download, entry); err != nil {
		return err
	}
	if _, err := lc.Register(ctx, req); err != nil {
		return fmt.Errorf("register attempt %s of %s: %w", a.ID, za.Name, err)
	}
	return nil
}

// fetchSpec is what a fetch attempt pulls: the catalog entry's .meta4 URL, hash and size, or the spec URL and hash.
func (r *ZimArchiveReconciler) fetchSpec(ctx context.Context, d *v1alpha1.DownloadSource, entry *catalog.Entry) (*libapi.FetchSpec, error) {
	if d.Catalog == nil {
		return &libapi.FetchSpec{URL: d.URL, SHA256: d.SHA256}, nil
	}
	if entry == nil {
		e, err := r.Catalog.Latest(ctx, d.Catalog.Name, d.Catalog.Flavour)
		if err != nil {
			return nil, fmt.Errorf("look up %s in the catalog: %w", d.Catalog.Name, err)
		}
		entry = &e
	}
	return &libapi.FetchSpec{URL: entry.URL, SHA256: entry.SHA256, Size: entry.Size}, nil
}

// ensureJob creates the crawl Job, owned by the archive, records jobCreated, then creates the customCss ConfigMap owned by the Job.
func (r *ZimArchiveReconciler) ensureJob(ctx context.Context, za *v1alpha1.ZimArchive, lib *v1alpha1.KiwixLibrary, before *v1alpha1.ZimArchiveStatus) error {
	a := za.Status.LastAttempt
	job, cm := buildCrawlJob(za, lib, a.ID, r.ZimitImage, r.OperatorImage)
	if !a.JobCreated {
		if err := r.createOwned(ctx, za, job); err != nil {
			return err
		}
		a.JobCreated = true
		if err := r.saveStatus(ctx, za, before); err != nil {
			return err
		}
	}
	if cm == nil {
		return nil
	}
	var owner batchv1.Job
	if err := r.Get(ctx, client.ObjectKeyFromObject(job), &owner); err != nil {
		return fmt.Errorf("get crawl Job %s: %w", job.Name, err)
	}
	return r.createOwned(ctx, &owner, cm)
}

// createOwned creates obj with owner as its controller unless it already exists.
func (r *ZimArchiveReconciler) createOwned(ctx context.Context, owner, obj client.Object) error {
	err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj.DeepCopyObject().(client.Object))
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get %T %s: %w", obj, obj.GetName(), err)
	}
	if err := controllerutil.SetControllerReference(owner, obj, r.Scheme); err != nil {
		return fmt.Errorf("own %T %s: %w", obj, obj.GetName(), err)
	}
	if err := r.Create(ctx, obj); client.IgnoreAlreadyExists(err) != nil {
		return fmt.Errorf("create %T %s: %w", obj, obj.GetName(), err)
	}
	return nil
}

func setIdle(za *v1alpha1.ZimArchive) {
	setArchiveCondition(za, v1alpha1.ConditionProgressing, metav1.ConditionFalse, v1alpha1.ReasonIdle, "no attempt running")
}

// clearDegraded sets Degraded to Healthy when it is unset or set for one of reasons, which the caller has just found resolved.
func clearDegraded(za *v1alpha1.ZimArchive, reasons ...string) {
	c := meta.FindStatusCondition(za.Status.Conditions, v1alpha1.ConditionDegraded)
	if c == nil || (c.Status == metav1.ConditionTrue && slices.Contains(reasons, c.Reason)) {
		setArchiveCondition(za, v1alpha1.ConditionDegraded, metav1.ConditionFalse, v1alpha1.ReasonHealthy, "no problem found")
	}
}

// refreshAfter is the wait until next, capped at a day; zero next means no refresh is scheduled.
func refreshAfter(next, now time.Time) time.Duration {
	if next.IsZero() {
		return maxRefreshRequeue
	}
	return min(next.Sub(now), maxRefreshRequeue)
}

func optTime(t time.Time) *metav1.Time {
	if t.IsZero() {
		return nil
	}
	return new(metav1.NewTime(t))
}
