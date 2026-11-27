package controller

import (
	"cmp"
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/jacaudi/kiwix-operator/api/v1alpha1"
)

const (
	// finalizerRetry is how often a pending or failed librarian delete is retried.
	finalizerRetry = 10 * time.Second
	// finalizerGiveUp is how long after the first failed or pending delete the finalizer is removed anyway.
	finalizerGiveUp = 10 * time.Minute
)

// finalize deletes the archive from its library under spec.deletionPolicy, then removes the finalizer; it gives up finalizerGiveUp after the first failed or pending delete.
func (r *ZimArchiveReconciler) finalize(ctx context.Context, za *v1alpha1.ZimArchive, lib *v1alpha1.KiwixLibrary) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(za, v1alpha1.FinalizerName) {
		return ctrl.Result{}, nil
	}
	if lib == nil {
		r.Recorder.Eventf(za, nil, corev1.EventTypeWarning, "LibraryGone", "Finalize",
			"KiwixLibrary %s is absent or being deleted; %s was not deleted from it", za.Spec.LibraryRef.Name, za.Name)
		return ctrl.Result{}, r.removeFinalizer(ctx, za)
	}
	done, err := r.deleteFromLibrary(ctx, za, lib)
	if done {
		return ctrl.Result{}, r.removeFinalizer(ctx, za)
	}
	why := "the librarian is still removing the file"
	if err != nil {
		// Not returned: the give-up clock, not the error backoff, decides when to stop.
		ctrl.LoggerFrom(ctx).Error(err, "delete archive from the librarian", "archive", za.Name)
		why = err.Error()
	}
	now := r.now()
	if za.Status.FinalizerFirstFailureAt == nil {
		before := za.Status.DeepCopy()
		za.Status.FinalizerFirstFailureAt = new(metav1.NewTime(now))
		if err := r.saveStatus(ctx, za, before); err != nil {
			return ctrl.Result{}, err
		}
	}
	if now.Sub(za.Status.FinalizerFirstFailureAt.Time) >= finalizerGiveUp {
		r.Recorder.Eventf(za, nil, corev1.EventTypeWarning, "FinalizerGaveUp", "Finalize",
			"deleting %s from KiwixLibrary %s did not finish within %s: %s", za.Name, lib.Name, finalizerGiveUp, why)
		return ctrl.Result{}, r.removeFinalizer(ctx, za)
	}
	return ctrl.Result{RequeueAfter: finalizerRetry}, nil
}

// deleteFromLibrary asks the librarian to delete the archive; done is false while it waits for kiwix-serve to drop the book.
func (r *ZimArchiveReconciler) deleteFromLibrary(ctx context.Context, za *v1alpha1.ZimArchive, lib *v1alpha1.KiwixLibrary) (bool, error) {
	lc, err := r.librarian(ctx, lib)
	if err != nil {
		return false, err
	}
	// Admission defaults deletionPolicy; an object stored without one keeps its file.
	policy := cmp.Or(za.Spec.DeletionPolicy, v1alpha1.DeletionPolicyRetain)
	done, err := lc.Delete(ctx, za.Name, string(policy))
	if err != nil {
		return false, fmt.Errorf("delete archive %s from the librarian: %w", za.Name, err)
	}
	return done, nil
}

func (r *ZimArchiveReconciler) removeFinalizer(ctx context.Context, za *v1alpha1.ZimArchive) error {
	controllerutil.RemoveFinalizer(za, v1alpha1.FinalizerName)
	if err := r.Update(ctx, za); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("remove finalizer from ZimArchive %s/%s: %w", za.Namespace, za.Name, err)
	}
	return nil
}
