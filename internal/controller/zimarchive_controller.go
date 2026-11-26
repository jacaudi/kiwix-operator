package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/jacaudi/kiwix-operator/api/v1alpha1"
	"github.com/jacaudi/kiwix-operator/internal/catalog"
	"github.com/jacaudi/kiwix-operator/internal/libapi"
	"github.com/jacaudi/kiwix-operator/internal/notify"
)

// libraryRefField indexes ZimArchives by the library they belong to.
const libraryRefField = "spec.libraryRef.name"

// Requeue intervals of the ZimArchive controller.
const (
	openAttemptPoll   = 15 * time.Second
	queuedRetry       = time.Minute
	catalogRetry      = time.Hour
	maxRefreshRequeue = 24 * time.Hour
)

// ArchiveLibrarian is the librarian API the ZimArchive controller calls; *libclient.Client implements it, and its sentinels classify the errors.
type ArchiveLibrarian interface {
	Register(ctx context.Context, req libapi.RegisterRequest) (libapi.AttemptState, error)
	Get(ctx context.Context, attempt string) (libapi.AttemptState, error)
	Promote(ctx context.Context, attempt string, req libapi.PromoteRequest) (libapi.BookInfo, error)
	Discard(ctx context.Context, attempt string) (libapi.AttemptState, error)
	Archive(ctx context.Context, archive string) (*libapi.BookInfo, error)
	Adopt(ctx context.Context, archive string, req libapi.AdoptRequest) (libapi.BookInfo, error)
	Delete(ctx context.Context, archive, policy string) (done bool, err error)
}

// ZimArchiveReconciler mirrors each ZimArchive's current version from its library's librarian and starts crawl and download attempts when due.
type ZimArchiveReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	// OperatorImage runs the crawl pod's upload container; ZimitImage runs its crawl.
	OperatorImage, ZimitImage string
	Catalog                   *catalog.Client
	Notifier                  *notify.Notifier
	// NewLibrarian returns a client of lib's librarian that sends the admin token.
	NewLibrarian func(lib *v1alpha1.KiwixLibrary, token string) ArchiveLibrarian
	// Now defaults to time.Now.
	Now func() time.Time

	// apiReader lists ZimArchives uncached for the concurrency count; SetupWithManager sets it.
	apiReader client.Reader
}

// +kubebuilder:rbac:groups=kiwix.jacaudi.dev,resources=zimarchives,verbs=get;list;watch
// +kubebuilder:rbac:groups=kiwix.jacaudi.dev,resources=zimarchives/status,verbs=get;update
// +kubebuilder:rbac:groups=kiwix.jacaudi.dev,resources=zimarchives/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=batch,resources=jobs/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create

// Reconcile resumes an archive's open attempt, or runs the start phase.
func (r *ZimArchiveReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var za v1alpha1.ZimArchive
	if err := r.Get(ctx, req.NamespacedName, &za); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	var lib v1alpha1.KiwixLibrary
	if err := r.Get(ctx, types.NamespacedName{Namespace: za.Namespace, Name: za.Spec.LibraryRef.Name}, &lib); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get KiwixLibrary %s/%s: %w", za.Namespace, za.Spec.LibraryRef.Name, err)
		}
		before := za.Status.DeepCopy()
		setArchiveCondition(&za, v1alpha1.ConditionAvailable, metav1.ConditionFalse, v1alpha1.ReasonLibraryUnavailable,
			fmt.Sprintf("KiwixLibrary %s not found", za.Spec.LibraryRef.Name))
		return ctrl.Result{RequeueAfter: statusPoll}, r.saveStatus(ctx, &za, before)
	}
	if attemptOpen(za.Status.LastAttempt) {
		return r.resume(ctx, &za, &lib)
	}
	return r.start(ctx, &za, &lib)
}

func (r *ZimArchiveReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// librarian returns a client of lib's librarian, authorized by the admin token in lib's admin Secret.
func (r *ZimArchiveReconciler) librarian(ctx context.Context, lib *v1alpha1.KiwixLibrary) (ArchiveLibrarian, error) {
	var s corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: lib.Namespace, Name: adminSecretName(lib)}, &s); err != nil {
		return nil, fmt.Errorf("read librarian admin Secret %s/%s: %w", lib.Namespace, adminSecretName(lib), err)
	}
	token := string(s.Data[adminTokenKey])
	if token == "" {
		return nil, fmt.Errorf("librarian admin Secret %s/%s has no %q key", lib.Namespace, adminSecretName(lib), adminTokenKey)
	}
	return r.NewLibrarian(lib, token), nil
}

// saveStatus writes za's status when it differs from before, then makes before the written status.
func (r *ZimArchiveReconciler) saveStatus(ctx context.Context, za *v1alpha1.ZimArchive, before *v1alpha1.ZimArchiveStatus) error {
	za.Status.ObservedGeneration = za.Generation
	if equality.Semantic.DeepEqual(before, &za.Status) {
		return nil
	}
	if err := r.Status().Update(ctx, za); err != nil {
		return fmt.Errorf("update ZimArchive %s/%s status: %w", za.Namespace, za.Name, err)
	}
	*before = *za.Status.DeepCopy()
	return nil
}

func setArchiveCondition(za *v1alpha1.ZimArchive, typ string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&za.Status.Conditions, metav1.Condition{
		Type: typ, Status: status, Reason: reason, Message: msg, ObservedGeneration: za.Generation,
	})
}

// attemptOpen reports whether a is an attempt still in progress.
func attemptOpen(a *v1alpha1.AttemptStatus) bool {
	return a != nil && a.ID != "" && a.FinishedAt == nil
}

func libraryRefOf(obj client.Object) []string {
	za, ok := obj.(*v1alpha1.ZimArchive)
	if !ok {
		return nil
	}
	return []string{za.Spec.LibraryRef.Name}
}

// archiveForPod maps a crawl pod to its ZimArchive by the archive label.
func archiveForPod(_ context.Context, obj client.Object) []reconcile.Request {
	name := obj.GetLabels()[v1alpha1.LabelArchive]
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
}

// archivesForLibrary maps a KiwixLibrary to every ZimArchive in its namespace that references it.
func (r *ZimArchiveReconciler) archivesForLibrary(ctx context.Context, obj client.Object) []reconcile.Request {
	var list v1alpha1.ZimArchiveList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace()), client.MatchingFields{libraryRefField: obj.GetName()}); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "list ZimArchives of KiwixLibrary", "library", obj.GetName())
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for _, za := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&za)})
	}
	return reqs
}

// SetupWithManager registers the controller with one worker, so the concurrency count never races itself.
func (r *ZimArchiveReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.apiReader = mgr.GetAPIReader()
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &v1alpha1.ZimArchive{}, libraryRefField, libraryRefOf); err != nil {
		return fmt.Errorf("index ZimArchives by %s: %w", libraryRefField, err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ZimArchive{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.Secret{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(archiveForPod)).
		Watches(&v1alpha1.KiwixLibrary{}, handler.EnqueueRequestsFromMapFunc(r.archivesForLibrary)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named("zimarchive").
		Complete(r)
}
