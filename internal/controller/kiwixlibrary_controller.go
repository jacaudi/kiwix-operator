// Package controller holds the operator's reconcilers.
package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jacaudi/kiwix-operator/api/v1alpha1"
	"github.com/jacaudi/kiwix-operator/internal/libapi"
)

// statusPoll is how often a library's status is refreshed from its librarian.
const statusPoll = time.Minute

// LibraryStatusAPI is the librarian call the KiwixLibrary controller makes; *libclient.Client implements it.
type LibraryStatusAPI interface {
	Library(ctx context.Context) (libapi.Library, error)
}

// KiwixLibraryReconciler turns a KiwixLibrary into its kiwix pod, Services, admin Secret, PVC, NetworkPolicies and HTTPRoute, and reports the librarian's view in status.
type KiwixLibraryReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	// OperatorImage runs the librarian and kiwix-serve.
	OperatorImage string
	// OperatorNamespace is where the manager's pods run.
	OperatorNamespace string
	// EgressAllowCIDRs are added to the kiwix and crawl pods' internet egress rules.
	EgressAllowCIDRs []string
	// ProbeAllowCIDRs may reach both kiwix ports, for policy implementations that filter kubelet probes.
	ProbeAllowCIDRs []string
	// GatewayAPIAvailable is whether the cluster serves HTTPRoutes.
	GatewayAPIAvailable bool
	// NewLibClient returns a client of the librarian at baseURL that sends the admin token.
	NewLibClient func(baseURL, token string) LibraryStatusAPI
}

// +kubebuilder:rbac:groups=kiwix.jacaudi.dev,resources=kiwixlibraries,verbs=get;list;watch
// +kubebuilder:rbac:groups=kiwix.jacaudi.dev,resources=kiwixlibraries/status,verbs=get;update
// +kubebuilder:rbac:groups=kiwix.jacaudi.dev,resources=kiwixlibraries/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=services;secrets;persistentvolumeclaims,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile applies a library's objects and refreshes its status; it always polls again after statusPoll.
func (r *KiwixLibraryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var lib v1alpha1.KiwixLibrary
	if err := r.Get(ctx, req.NamespacedName, &lib); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	before := lib.Status.DeepCopy()

	holder, err := r.claimHolder(ctx, &lib)
	if err != nil {
		return ctrl.Result{}, err
	}
	token, err := r.applySecret(ctx, &lib)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.applyStorageAndNetwork(ctx, &lib); err != nil {
		return ctrl.Result{}, err
	}
	routeUnsupported, err := r.applyRoute(ctx, &lib)
	if err != nil {
		return ctrl.Result{}, err
	}

	var library *libapi.Library
	if holder == "" {
		ready, err := r.applyDeployment(ctx, &lib)
		if err != nil {
			return ctrl.Result{}, err
		}
		library = r.refreshAvailable(ctx, &lib, ready, token)
	} else if err := r.deleteOwned(ctx, &lib, &appsv1.Deployment{ObjectMeta: objectMeta(&lib, lib.Name, nil)}); err != nil {
		return ctrl.Result{}, err
	}
	r.setDegraded(&lib, before, holder, routeUnsupported, library)

	lib.Status.ObservedGeneration = lib.Generation
	if !equality.Semantic.DeepEqual(before, &lib.Status) {
		if err := r.Status().Update(ctx, &lib); err != nil {
			return ctrl.Result{}, fmt.Errorf("update KiwixLibrary %s status: %w", req.NamespacedName, err)
		}
	}
	return ctrl.Result{RequeueAfter: statusPoll}, nil
}

// apply creates or updates obj with the library's labels, as its controller when owned; mutate sets the fields the operator manages.
func (r *KiwixLibraryReconciler) apply(ctx context.Context, lib *v1alpha1.KiwixLibrary, obj client.Object, owned bool, mutate func() error) error {
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
		// An existing object is only updated if the operator made it for this library; a same-named object of anyone else is left as it is.
		if obj.GetResourceVersion() != "" && !madeForLibrary(lib, obj, owned) {
			return errors.New("it exists and was not made by the operator for this KiwixLibrary")
		}
		labels := obj.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		maps.Copy(labels, podLabels(lib))
		obj.SetLabels(labels)
		if err := mutate(); err != nil {
			return err
		}
		if !owned {
			return nil
		}
		return controllerutil.SetControllerReference(lib, obj, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("apply %T %s/%s: %w", obj, lib.Namespace, obj.GetName(), err)
	}
	return nil
}

// madeForLibrary reports whether the existing obj was made by the operator for lib: lib controls it, or, for an object that is never owned, it carries lib's instance label and the managed-by label.
func madeForLibrary(lib *v1alpha1.KiwixLibrary, obj client.Object, owned bool) bool {
	if owned {
		return metav1.IsControlledBy(obj, lib)
	}
	l := obj.GetLabels()
	return l[v1alpha1.LabelManagedBy] == v1alpha1.ManagedByValue && l[labelInstance] == lib.Name
}

// deleteOwned deletes obj if it exists and lib controls it.
func (r *KiwixLibraryReconciler) deleteOwned(ctx context.Context, lib *v1alpha1.KiwixLibrary, obj client.Object) error {
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(obj, lib) {
		return nil
	}
	if err := client.IgnoreNotFound(r.Delete(ctx, obj)); err != nil {
		return fmt.Errorf("delete %T %s/%s: %w", obj, lib.Namespace, obj.GetName(), err)
	}
	return nil
}

// claimHolder returns the name of the library that holds lib's claim when that is another library: the one created first, then the lowest name.
func (r *KiwixLibraryReconciler) claimHolder(ctx context.Context, lib *v1alpha1.KiwixLibrary) (string, error) {
	var list v1alpha1.KiwixLibraryList
	if err := r.List(ctx, &list, client.InNamespace(lib.Namespace)); err != nil {
		return "", fmt.Errorf("list KiwixLibraries in %s: %w", lib.Namespace, err)
	}
	claim := claimName(lib)
	holder := lib
	for i := range list.Items {
		other := &list.Items[i]
		if claimName(other) == claim && createdBefore(other, holder) {
			holder = other
		}
	}
	if holder.Name == lib.Name {
		return "", nil
	}
	return holder.Name, nil
}

func createdBefore(a, b *v1alpha1.KiwixLibrary) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Name < b.Name
}

// applySecret ensures the librarian admin Secret and returns its token, generated once and never rotated.
func (r *KiwixLibraryReconciler) applySecret(ctx context.Context, lib *v1alpha1.KiwixLibrary) (string, error) {
	s := &corev1.Secret{ObjectMeta: objectMeta(lib, adminSecretName(lib), nil)}
	err := r.apply(ctx, lib, s, true, func() error {
		if !s.CreationTimestamp.IsZero() {
			return nil
		}
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return fmt.Errorf("generate librarian admin token: %w", err)
		}
		s.Data = map[string][]byte{adminTokenKey: []byte(hex.EncodeToString(b))}
		return nil
	})
	return string(s.Data[adminTokenKey]), err
}

// applyStorageAndNetwork ensures the PVC (when spec.storage.size is set, never owned), both Services and both NetworkPolicies.
func (r *KiwixLibraryReconciler) applyStorageAndNetwork(ctx context.Context, lib *v1alpha1.KiwixLibrary) error {
	if size := lib.Spec.Storage.Size; size != nil {
		pvc := &corev1.PersistentVolumeClaim{ObjectMeta: objectMeta(lib, dataClaimName(lib), nil)}
		// No owner reference: deleting the library must never delete its ZIM files.
		err := r.apply(ctx, lib, pvc, false, func() error {
			if pvc.CreationTimestamp.IsZero() {
				pvc.Spec = corev1.PersistentVolumeClaimSpec{
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: *size}},
					StorageClassName: classOrNil(lib.Spec.Storage.StorageClassName),
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	serve, librarian := buildServices(lib)
	for _, want := range []*corev1.Service{serve, librarian} {
		svc := &corev1.Service{ObjectMeta: objectMeta(lib, want.Name, nil)}
		err := r.apply(ctx, lib, svc, true, func() error {
			svc.Spec.Ports = want.Spec.Ports
			svc.Spec.Selector = want.Spec.Selector
			svc.Spec.PublishNotReadyAddresses = want.Spec.PublishNotReadyAddresses
			return nil
		})
		if err != nil {
			return err
		}
	}

	for _, want := range buildNetworkPolicies(lib, r.OperatorNamespace, r.EgressAllowCIDRs, r.ProbeAllowCIDRs) {
		np := &networkingv1.NetworkPolicy{ObjectMeta: objectMeta(lib, want.Name, nil)}
		if err := r.apply(ctx, lib, np, true, func() error { np.Spec = want.Spec; return nil }); err != nil {
			return err
		}
	}
	return nil
}

// applyRoute ensures the HTTPRoute, or deletes it when spec.route is gone; it reports a route the cluster cannot serve.
func (r *KiwixLibraryReconciler) applyRoute(ctx context.Context, lib *v1alpha1.KiwixLibrary) (unsupported bool, err error) {
	switch {
	case !r.GatewayAPIAvailable:
		return lib.Spec.Route != nil, nil
	case lib.Spec.Route == nil:
		// An owner reference only cleans up when the library goes, not when its route field does.
		return false, r.deleteOwned(ctx, lib, &gwapiv1.HTTPRoute{ObjectMeta: objectMeta(lib, lib.Name, nil)})
	}
	want := buildHTTPRoute(lib)
	route := &gwapiv1.HTTPRoute{ObjectMeta: objectMeta(lib, want.Name, nil)}
	return false, r.apply(ctx, lib, route, true, func() error { route.Spec = want.Spec; return nil })
}

// applyDeployment ensures the kiwix pod's Deployment and reports whether it has a ready replica.
func (r *KiwixLibraryReconciler) applyDeployment(ctx context.Context, lib *v1alpha1.KiwixLibrary) (ready bool, err error) {
	want := buildDeployment(lib, r.OperatorImage, claimName(lib))
	d := &appsv1.Deployment{ObjectMeta: objectMeta(lib, want.Name, nil)}
	err = r.apply(ctx, lib, d, true, func() error {
		if d.CreationTimestamp.IsZero() {
			d.Spec = want.Spec
			return nil
		}
		d.Spec.Strategy = want.Spec.Strategy
		d.Spec.Template = want.Spec.Template
		return nil
	})
	return d.Status.ReadyReplicas > 0, err
}

func setCondition(lib *v1alpha1.KiwixLibrary, typ string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&lib.Status.Conditions, metav1.Condition{
		Type: typ, Status: status, Reason: reason, Message: msg, ObservedGeneration: lib.Generation,
	})
}

// refreshAvailable sets Available and, when the librarian answers, copies its summary into status and returns it.
func (r *KiwixLibraryReconciler) refreshAvailable(ctx context.Context, lib *v1alpha1.KiwixLibrary, ready bool, token string) *libapi.Library {
	if !ready {
		setCondition(lib, v1alpha1.ConditionAvailable, metav1.ConditionFalse, v1alpha1.ReasonStarting, "kiwix-serve has no ready replica")
		return nil
	}
	l, err := r.NewLibClient(LibrarianURL(lib), token).Library(ctx)
	if err != nil {
		setCondition(lib, v1alpha1.ConditionAvailable, metav1.ConditionFalse, v1alpha1.ReasonLibrarianUnreachable, err.Error())
		return nil
	}
	setCondition(lib, v1alpha1.ConditionAvailable, metav1.ConditionTrue, v1alpha1.ReasonServing, fmt.Sprintf("serving %d books", len(l.Books)))
	lib.Status.Books = int32(min(len(l.Books), math.MaxInt32))
	lib.Status.FreeBytes = l.FreeBytes
	lib.Status.UnmanagedFiles = nil
	for _, u := range l.Unmanaged {
		lib.Status.UnmanagedFiles = append(lib.Status.UnmanagedFiles, v1alpha1.UnmanagedFile{Name: u.Name, Error: u.Error})
	}
	return &l
}

// setDegraded sets Degraded to the first applicable problem, or Healthy, and records a Warning Event when the problem changes.
func (r *KiwixLibraryReconciler) setDegraded(lib *v1alpha1.KiwixLibrary, before *v1alpha1.KiwixLibraryStatus, holder string, routeUnsupported bool, l *libapi.Library) {
	reason, msg := degradedReason(lib, holder, routeUnsupported, l)
	if reason == v1alpha1.ReasonHealthy {
		setCondition(lib, v1alpha1.ConditionDegraded, metav1.ConditionFalse, reason, msg)
		return
	}
	setCondition(lib, v1alpha1.ConditionDegraded, metav1.ConditionTrue, reason, msg)
	if old := meta.FindStatusCondition(before.Conditions, v1alpha1.ConditionDegraded); old == nil || old.Reason != reason {
		r.Recorder.Eventf(lib, nil, corev1.EventTypeWarning, reason, "Reconcile", "%s", msg)
	}
}

// degradedReason picks the first of ClaimInUse, RouteUnsupported, ReloadUnconfirmed, LowSpace and UnmanagedFileErrors; l is nil when the librarian was not read.
func degradedReason(lib *v1alpha1.KiwixLibrary, holder string, routeUnsupported bool, l *libapi.Library) (reason, msg string) {
	switch {
	case holder != "":
		return v1alpha1.ReasonClaimInUse, fmt.Sprintf("claim %s is already used by KiwixLibrary %s", claimName(lib), holder)
	case routeUnsupported:
		return v1alpha1.ReasonRouteUnsupported, "spec.route is set but the cluster does not serve Gateway API HTTPRoutes"
	case l == nil:
		return v1alpha1.ReasonHealthy, "no problem found"
	case l.ReloadUnconfirmed:
		return v1alpha1.ReasonReloadUnconfirmed, "kiwix-serve did not confirm the last library.xml change"
	}
	if reserve := lib.Spec.ReserveSpace.Value(); l.FreeBytes < 2*reserve {
		return v1alpha1.ReasonLowSpace, fmt.Sprintf("%d bytes free, below twice the %d-byte reserve", l.FreeBytes, reserve)
	}
	broken := 0
	for _, u := range l.Unmanaged {
		if u.Error != "" {
			broken++
		}
	}
	if broken > 0 {
		return v1alpha1.ReasonUnmanagedFileErrors, fmt.Sprintf("%d unmanaged files could not be served; see status.unmanagedFiles", broken)
	}
	return v1alpha1.ReasonHealthy, "no problem found"
}

// specChangedOnly drops update events that left the spec alone, so a status write never re-triggers the reconcile that made it.
var specChangedOnly = predicate.GenerationChangedPredicate{}

// SetupWithManager registers the controller; it watches HTTPRoutes only when the cluster serves them.
func (r *KiwixLibraryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.KiwixLibrary{}, builder.WithPredicates(specChangedOnly)).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Owns(&networkingv1.NetworkPolicy{})
	if r.GatewayAPIAvailable {
		b = b.Owns(&gwapiv1.HTTPRoute{})
	}
	return b.Named("kiwixlibrary").Complete(r)
}
