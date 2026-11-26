package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jacaudi/kiwix-operator/api/v1alpha1"
	"github.com/jacaudi/kiwix-operator/internal/libapi"
)

const testImage = "ghcr.io/jacaudi/kiwix-operator:test"

func testLib() *v1alpha1.KiwixLibrary {
	return &v1alpha1.KiwixLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "wiki", Namespace: "kiwix", Generation: 3, UID: "uid-wiki"},
		Spec: v1alpha1.KiwixLibrarySpec{
			Storage:      v1alpha1.StorageSpec{Size: new(resource.MustParse("10Gi"))},
			Downloads:    v1alpha1.ConcurrencySpec{MaxConcurrent: 2},
			Crawls:       v1alpha1.ConcurrencySpec{MaxConcurrent: 1},
			ReserveSpace: resource.MustParse("5Gi"),
		},
	}
}

func mountAt(t *testing.T, c corev1.Container, path string) corev1.VolumeMount {
	t.Helper()
	for _, m := range c.VolumeMounts {
		if m.MountPath == path {
			return m
		}
	}
	t.Fatalf("container %s has no mount at %s", c.Name, path)
	return corev1.VolumeMount{}
}

func volume(t *testing.T, spec corev1.PodSpec, name string) corev1.Volume {
	t.Helper()
	for _, v := range spec.Volumes {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("pod has no volume %s", name)
	return corev1.Volume{}
}

func TestDeploymentSidecarAndProbes(t *testing.T) {
	d := buildDeployment(testLib(), testImage, "wiki-data")
	assert.Equal(t, "wiki", d.Name)
	assert.Equal(t, "kiwix", d.Namespace)
	spec := d.Spec.Template.Spec

	require.Len(t, spec.InitContainers, 1)
	lib := spec.InitContainers[0]
	assert.Equal(t, "librarian", lib.Name)
	assert.Equal(t, testImage, lib.Image)
	assert.Equal(t, new(corev1.ContainerRestartPolicyAlways), lib.RestartPolicy)
	require.NotEmpty(t, lib.Args)
	assert.Equal(t, "librarian", lib.Args[0])
	require.NotNil(t, lib.StartupProbe)
	assert.Equal(t, "/healthz/startup", lib.StartupProbe.HTTPGet.Path)
	assert.Equal(t, intstr.FromInt32(8090), lib.StartupProbe.HTTPGet.Port)
	assert.EqualValues(t, 5, lib.StartupProbe.PeriodSeconds)
	assert.EqualValues(t, 360, lib.StartupProbe.FailureThreshold)
	require.NotNil(t, lib.LivenessProbe)
	assert.Equal(t, "/healthz/live", lib.LivenessProbe.HTTPGet.Path)
	assert.Equal(t, intstr.FromInt32(8090), lib.LivenessProbe.HTTPGet.Port)
	assert.Nil(t, lib.ReadinessProbe, "a sidecar readinessProbe would gate kiwix-serve's Service")
	assert.False(t, mountAt(t, lib, "/data").ReadOnly)

	require.Len(t, spec.Containers, 1)
	srv := spec.Containers[0]
	assert.Equal(t, "kiwix-serve", srv.Name)
	assert.Equal(t, testImage, srv.Image)
	assert.Equal(t, []string{"/usr/local/bin/kiwix-serve"}, srv.Command)
	assert.Equal(t, []string{"--library", "/data/library.xml", "--monitorLibrary", "--port", "8080"}, srv.Args)
	require.NotNil(t, srv.ReadinessProbe)
	assert.Equal(t, "/catalog/search?count=1", srv.ReadinessProbe.HTTPGet.Path)
	assert.Equal(t, intstr.FromInt32(8080), srv.ReadinessProbe.HTTPGet.Port)
	assert.True(t, mountAt(t, srv, "/data").ReadOnly)

	assert.Equal(t, new(int64(10)), spec.TerminationGracePeriodSeconds)
	data := volume(t, spec, mountAt(t, srv, "/data").Name)
	require.NotNil(t, data.PersistentVolumeClaim)
	assert.Equal(t, "wiki-data", data.PersistentVolumeClaim.ClaimName)
}

func TestDeploymentLibrarianArgs(t *testing.T) {
	lib := buildDeployment(testLib(), testImage, "wiki-data").Spec.Template.Spec.InitContainers[0]

	assert.Equal(t, []string{
		"librarian",
		"--data-dir=/data",
		"--listen=:8090",
		"--kiwix-serve-url=http://127.0.0.1:8080",
		"--reserve-space=5368709120",
		"--max-fetches=2",
		"--kiwix-manage=/usr/local/bin/kiwix-manage",
	}, lib.Args)
	assert.Equal(t, []corev1.EnvVar{{
		Name: "KIWIX_LIBRARIAN_ADMIN_TOKEN",
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "wiki-librarian-admin"},
			Key:                  "token",
		}},
	}}, lib.Env)
	assert.Equal(t, corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
	}, lib.Resources)
}

func TestDeploymentServerResources(t *testing.T) {
	l := testLib()
	l.Spec.Server.Resources = corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")}}

	srv := buildDeployment(l, testImage, "wiki-data").Spec.Template.Spec.Containers[0]

	assert.Equal(t, l.Spec.Server.Resources, srv.Resources)
}

func TestDeploymentBlockExternal(t *testing.T) {
	base := []string{"--library", "/data/library.xml", "--monitorLibrary", "--port", "8080"}
	tests := []struct {
		name  string
		block bool
		want  []string
	}{
		{name: "off", block: false, want: base},
		{name: "on", block: true, want: append(append([]string(nil), base...), "--blockexternal")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := testLib()
			l.Spec.Server.BlockExternalLinks = tt.block

			got := buildDeployment(l, testImage, "wiki-data").Spec.Template.Spec.Containers[0].Args

			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDeploymentSecurity(t *testing.T) {
	d := buildDeployment(testLib(), testImage, "wiki-data")
	spec := d.Spec.Template.Spec

	assert.Equal(t, appsv1.RecreateDeploymentStrategyType, d.Spec.Strategy.Type)
	assert.Equal(t, new(false), spec.AutomountServiceAccountToken)
	assert.Equal(t, &corev1.PodSecurityContext{
		RunAsNonRoot:        new(true),
		RunAsUser:           new(int64(1000)),
		RunAsGroup:          new(int64(1000)),
		FSGroup:             new(int64(1000)),
		FSGroupChangePolicy: new(corev1.FSGroupChangeOnRootMismatch),
		SeccompProfile:      &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}, spec.SecurityContext)
	tmp := volume(t, spec, "tmp")
	assert.NotNil(t, tmp.EmptyDir)

	for _, c := range append(spec.InitContainers, spec.Containers...) {
		t.Run(c.Name, func(t *testing.T) {
			require.NotNil(t, c.SecurityContext)
			assert.Equal(t, new(false), c.SecurityContext.AllowPrivilegeEscalation)
			assert.Equal(t, new(true), c.SecurityContext.ReadOnlyRootFilesystem)
			assert.Equal(t, &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}, c.SecurityContext.Capabilities)
			assert.Equal(t, "tmp", mountAt(t, c, "/tmp").Name)
		})
	}
}

func TestDeploymentLabels(t *testing.T) {
	d := buildDeployment(testLib(), testImage, "wiki-data")
	want := map[string]string{
		"app.kubernetes.io/name":       "kiwix",
		"app.kubernetes.io/instance":   "wiki",
		"app.kubernetes.io/managed-by": "kiwix-operator",
	}

	assert.Equal(t, want, d.Labels)
	assert.Equal(t, want, d.Spec.Template.Labels)
	require.NotNil(t, d.Spec.Selector)
	assert.Equal(t, map[string]string{"app.kubernetes.io/name": "kiwix", "app.kubernetes.io/instance": "wiki"}, d.Spec.Selector.MatchLabels)
}

func TestLibrarianServicePublishesNotReady(t *testing.T) {
	selector := map[string]string{"app.kubernetes.io/name": "kiwix", "app.kubernetes.io/instance": "wiki"}

	serve, librarian := buildServices(testLib())

	assert.Equal(t, "wiki", serve.Name)
	assert.Equal(t, "kiwix", serve.Namespace)
	assert.Equal(t, selector, serve.Spec.Selector)
	assert.Equal(t, []corev1.ServicePort{{Name: "http", Protocol: corev1.ProtocolTCP, Port: 8080, TargetPort: intstr.FromInt32(8080)}}, serve.Spec.Ports)
	assert.False(t, serve.Spec.PublishNotReadyAddresses)
	assert.Equal(t, "kiwix-operator", serve.Labels["app.kubernetes.io/managed-by"])

	assert.Equal(t, "wiki-librarian", librarian.Name)
	assert.Equal(t, selector, librarian.Spec.Selector)
	assert.Equal(t, []corev1.ServicePort{{Name: "librarian", Protocol: corev1.ProtocolTCP, Port: 8090, TargetPort: intstr.FromInt32(8090)}}, librarian.Spec.Ports)
	assert.True(t, librarian.Spec.PublishNotReadyAddresses)
	assert.Equal(t, "kiwix-operator", librarian.Labels["app.kubernetes.io/managed-by"])
}

func TestHTTPRouteNumericPort(t *testing.T) {
	l := testLib()
	l.Spec.Route = &v1alpha1.RouteSpec{
		ParentRefs: []gwapiv1.ParentReference{{Name: "internal", Namespace: new(gwapiv1.Namespace("network")), SectionName: new(gwapiv1.SectionName("https"))}},
		Hostnames:  []gwapiv1.Hostname{"wiki.example.com"},
	}

	r := buildHTTPRoute(l)

	assert.Equal(t, "wiki", r.Name)
	assert.Equal(t, "kiwix", r.Namespace)
	assert.Equal(t, "kiwix-operator", r.Labels["app.kubernetes.io/managed-by"])
	assert.Equal(t, l.Spec.Route.ParentRefs, r.Spec.ParentRefs)
	assert.Equal(t, l.Spec.Route.Hostnames, r.Spec.Hostnames)
	require.Len(t, r.Spec.Rules, 1)
	require.Len(t, r.Spec.Rules[0].BackendRefs, 1)
	ref := r.Spec.Rules[0].BackendRefs[0]
	assert.Equal(t, gwapiv1.ObjectName("wiki"), ref.Name)
	assert.Equal(t, new(gwapiv1.PortNumber(8080)), ref.Port)
}

// fakeLibrarian answers Library with a canned reply and records the client it was built for.
type fakeLibrarian struct {
	reply          libapi.Library
	err            error
	calls          int
	baseURL, token string
}

func (f *fakeLibrarian) Library(context.Context) (libapi.Library, error) {
	f.calls++
	return f.reply, f.err
}

type harness struct {
	r             *KiwixLibraryReconciler
	c             client.Client
	librarian     *fakeLibrarian
	rec           *events.FakeRecorder
	statusUpdates int
}

func newHarness(t *testing.T, objs ...client.Object) *harness {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, gwapiv1.Install(scheme))
	h := &harness{librarian: &fakeLibrarian{}, rec: events.NewFakeRecorder(10)}
	h.c = fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&v1alpha1.KiwixLibrary{}, &appsv1.Deployment{}).
		WithInterceptorFuncs(interceptor.Funcs{
			// The API server stamps creationTimestamp on create; the fake client does not.
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				obj.SetCreationTimestamp(metav1.Now())
				return c.Create(ctx, obj, opts...)
			},
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if _, ok := obj.(*v1alpha1.KiwixLibrary); ok {
					h.statusUpdates++
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).
		Build()
	h.r = &KiwixLibraryReconciler{
		Client:              h.c,
		Scheme:              scheme,
		Recorder:            h.rec,
		OperatorImage:       testImage,
		OperatorNamespace:   "kiwix-system",
		GatewayAPIAvailable: true,
		NewLibClient: func(baseURL, token string) LibraryStatusAPI {
			h.librarian.baseURL, h.librarian.token = baseURL, token
			return h.librarian
		},
	}
	return h
}

func key(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: "kiwix", Name: name}
}

func (h *harness) reconcile(t *testing.T, name string) ctrl.Result {
	t.Helper()
	res, err := h.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key(name)})
	require.NoError(t, err)
	return res
}

func (h *harness) get(t *testing.T, name string, obj client.Object) {
	t.Helper()
	require.NoError(t, h.c.Get(t.Context(), key(name), obj))
}

func (h *harness) absent(t *testing.T, name string, obj client.Object) {
	t.Helper()
	err := h.c.Get(t.Context(), key(name), obj)
	assert.True(t, apierrors.IsNotFound(err), "%T %s: want NotFound, got %v", obj, name, err)
}

func (h *harness) markReady(t *testing.T, name string) {
	t.Helper()
	var d appsv1.Deployment
	h.get(t, name, &d)
	d.Status.ReadyReplicas = 1
	require.NoError(t, h.c.Status().Update(t.Context(), &d))
}

func (h *harness) token(t *testing.T, lib string) string {
	t.Helper()
	var s corev1.Secret
	h.get(t, lib+"-librarian-admin", &s)
	return string(s.Data["token"])
}

func (h *harness) library(t *testing.T, name string) *v1alpha1.KiwixLibrary {
	t.Helper()
	var l v1alpha1.KiwixLibrary
	h.get(t, name, &l)
	return &l
}

func assertCondition(t *testing.T, lib *v1alpha1.KiwixLibrary, typ string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	c := meta.FindStatusCondition(lib.Status.Conditions, typ)
	require.NotNil(t, c, "condition %s missing", typ)
	assert.Equal(t, status, c.Status, typ)
	assert.Equal(t, reason, c.Reason, typ)
	assert.Equal(t, lib.Generation, c.ObservedGeneration, typ)
}

func withRoute(l *v1alpha1.KiwixLibrary) *v1alpha1.KiwixLibrary {
	l.Spec.Route = &v1alpha1.RouteSpec{ParentRefs: []gwapiv1.ParentReference{{Name: "internal"}}, Hostnames: []gwapiv1.Hostname{"wiki.example.com"}}
	return l
}

func TestReconcileCreatesResources(t *testing.T) {
	lib := withRoute(testLib())
	h := newHarness(t, lib)

	res := h.reconcile(t, "wiki")

	assert.Equal(t, time.Minute, res.RequeueAfter)
	owned := map[string]client.Object{
		"wiki":                 &appsv1.Deployment{},
		"wiki-librarian-admin": &corev1.Secret{},
		"wiki-kiwix":           &networkingv1.NetworkPolicy{},
		"wiki-crawl":           &networkingv1.NetworkPolicy{},
	}
	for name, obj := range owned {
		h.get(t, name, obj)
		assert.Equal(t, "kiwix-operator", obj.GetLabels()["app.kubernetes.io/managed-by"], "%T %s", obj, name)
		assert.True(t, metav1.IsControlledBy(obj, lib), "%T %s is not controlled by the library", obj, name)
	}
	for _, obj := range []client.Object{&corev1.Service{}, &gwapiv1.HTTPRoute{}} {
		h.get(t, "wiki", obj)
		assert.Equal(t, "kiwix-operator", obj.GetLabels()["app.kubernetes.io/managed-by"], "%T", obj)
		assert.True(t, metav1.IsControlledBy(obj, lib), "%T is not controlled by the library", obj)
	}
	var librarianSvc corev1.Service
	h.get(t, "wiki-librarian", &librarianSvc)
	assert.True(t, metav1.IsControlledBy(&librarianSvc, lib))
	assert.Equal(t, "kiwix-operator", librarianSvc.Labels["app.kubernetes.io/managed-by"])

	var pvc corev1.PersistentVolumeClaim
	h.get(t, "wiki-data", &pvc)
	assert.Empty(t, pvc.OwnerReferences, "deleting the library must never delete its data")
	assert.Equal(t, "kiwix-operator", pvc.Labels["app.kubernetes.io/managed-by"])
	assert.Equal(t, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, pvc.Spec.AccessModes)
	assert.Equal(t, resource.MustParse("10Gi"), pvc.Spec.Resources.Requests[corev1.ResourceStorage])
	assert.Nil(t, pvc.Spec.StorageClassName, "an unset class must not be sent as \"\"")

	assert.Regexp(t, `^[0-9a-f]{64}$`, h.token(t, "wiki"))
}

func TestReconcilePVCStorageClass(t *testing.T) {
	lib := testLib()
	lib.Spec.Storage.StorageClassName = new("ceph-block")
	h := newHarness(t, lib)

	h.reconcile(t, "wiki")

	var pvc corev1.PersistentVolumeClaim
	h.get(t, "wiki-data", &pvc)
	assert.Equal(t, new("ceph-block"), pvc.Spec.StorageClassName)
}

func TestNetworkPolicyUpdatedOnFlagChange(t *testing.T) {
	h := newHarness(t, testLib())
	internetPeers := func(name string) int {
		var np networkingv1.NetworkPolicy
		h.get(t, name, &np)
		return len(np.Spec.Egress[len(np.Spec.Egress)-1].To)
	}

	h.r.EgressAllowCIDRs = []string{"10.244.0.0/16", "10.96.0.0/12"}
	h.reconcile(t, "wiki")
	assert.Equal(t, 3, internetPeers("wiki-kiwix"))
	assert.Equal(t, 3, internetPeers("wiki-crawl"))

	h.r.EgressAllowCIDRs = nil
	h.reconcile(t, "wiki")
	assert.Equal(t, 1, internetPeers("wiki-kiwix"))
	assert.Equal(t, 1, internetPeers("wiki-crawl"))
}

func TestReconcileStarting(t *testing.T) {
	h := newHarness(t, testLib())

	h.reconcile(t, "wiki")

	lib := h.library(t, "wiki")
	assertCondition(t, lib, v1alpha1.ConditionAvailable, metav1.ConditionFalse, v1alpha1.ReasonStarting)
	assertCondition(t, lib, v1alpha1.ConditionDegraded, metav1.ConditionFalse, v1alpha1.ReasonHealthy)
	assert.Equal(t, int64(3), lib.Status.ObservedGeneration)
	assert.Zero(t, h.librarian.calls, "the librarian is not asked before the pod is ready")
}

func TestReconcileExistingClaim(t *testing.T) {
	lib := testLib()
	lib.Spec.Storage = v1alpha1.StorageSpec{ExistingClaim: "kiwix"}
	h := newHarness(t, lib)

	h.reconcile(t, "wiki")

	var pvcs corev1.PersistentVolumeClaimList
	require.NoError(t, h.c.List(t.Context(), &pvcs))
	assert.Empty(t, pvcs.Items)
	var d appsv1.Deployment
	h.get(t, "wiki", &d)
	assert.Equal(t, "kiwix", volume(t, d.Spec.Template.Spec, "data").PersistentVolumeClaim.ClaimName)
}

func TestReconcileClaimInUse(t *testing.T) {
	t0 := metav1.NewTime(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	t1 := metav1.NewTime(t0.Add(time.Hour))
	tests := []struct {
		name          string
		first, second metav1.Time
		winner, loser string
	}{
		{name: "older wins over a lower name", first: t1, second: t0, winner: "b", loser: "a"},
		{name: "same time, lower name wins", first: t0, second: t0, winner: "a", loser: "b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mk := func(name string, at metav1.Time) *v1alpha1.KiwixLibrary {
				l := withRoute(testLib())
				l.Name, l.UID, l.CreationTimestamp = name, types.UID("uid-"+name), at
				l.Spec.Storage = v1alpha1.StorageSpec{ExistingClaim: "shared"}
				return l
			}
			libs := map[string]*v1alpha1.KiwixLibrary{"a": mk("a", tt.first), "b": mk("b", tt.second)}
			stale := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
				Name: tt.loser, Namespace: "kiwix",
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(libs[tt.loser], v1alpha1.GroupVersion.WithKind("KiwixLibrary"))},
			}}
			h := newHarness(t, libs["a"], libs["b"], stale)
			h.r.GatewayAPIAvailable = false

			h.reconcile(t, "a")
			h.reconcile(t, "b")

			h.absent(t, tt.loser, &appsv1.Deployment{})
			h.get(t, tt.winner, &appsv1.Deployment{})
			h.get(t, tt.loser, &corev1.Service{})
			h.get(t, tt.loser+"-librarian-admin", &corev1.Secret{})
			h.get(t, tt.loser+"-kiwix", &networkingv1.NetworkPolicy{})
			loser := h.library(t, tt.loser)
			assertCondition(t, loser, v1alpha1.ConditionDegraded, metav1.ConditionTrue, v1alpha1.ReasonClaimInUse)
			assert.Nil(t, meta.FindStatusCondition(loser.Status.Conditions, v1alpha1.ConditionAvailable), "only Degraded is set on the loser")
			assertCondition(t, h.library(t, tt.winner), v1alpha1.ConditionDegraded, metav1.ConditionTrue, v1alpha1.ReasonRouteUnsupported)

			var got []string
			for len(h.rec.Events) > 0 {
				got = append(got, <-h.rec.Events)
			}
			assert.Contains(t, got, "Warning ClaimInUse claim shared is already used by KiwixLibrary "+tt.winner)
		})
	}
}

func TestReconcileRouteUnsupported(t *testing.T) {
	h := newHarness(t, withRoute(testLib()))
	h.r.GatewayAPIAvailable = false
	h.reconcile(t, "wiki")
	h.markReady(t, "wiki")
	h.librarian.reply = libapi.Library{FreeBytes: 100 << 30, ReloadUnconfirmed: true}

	h.reconcile(t, "wiki")

	lib := h.library(t, "wiki")
	assertCondition(t, lib, v1alpha1.ConditionDegraded, metav1.ConditionTrue, v1alpha1.ReasonRouteUnsupported)
	assertCondition(t, lib, v1alpha1.ConditionAvailable, metav1.ConditionTrue, v1alpha1.ReasonServing)
	h.get(t, "wiki-librarian", &corev1.Service{})
	var routes gwapiv1.HTTPRouteList
	require.NoError(t, h.c.List(t.Context(), &routes))
	assert.Empty(t, routes.Items)
}

func TestReconcileRouteRemovedDeletes(t *testing.T) {
	h := newHarness(t, withRoute(testLib()))
	h.reconcile(t, "wiki")
	h.get(t, "wiki", &gwapiv1.HTTPRoute{})

	lib := h.library(t, "wiki")
	lib.Spec.Route = nil
	require.NoError(t, h.c.Update(t.Context(), lib))
	h.reconcile(t, "wiki")

	h.absent(t, "wiki", &gwapiv1.HTTPRoute{})
}

func TestReconcileStatusFromLibrarian(t *testing.T) {
	books := []libapi.BookInfo{{File: "a.zim"}, {File: "b.zim"}}
	odd := libapi.UnmanagedFile{Name: "odd name.zim"}
	broken := libapi.UnmanagedFile{Name: "broken.zim", Error: "kiwix-manage add failed"}
	tests := []struct {
		name   string
		reply  libapi.Library
		reason string
	}{
		{name: "healthy", reply: libapi.Library{Books: books, FreeBytes: 100 << 30, Unmanaged: []libapi.UnmanagedFile{odd}}, reason: v1alpha1.ReasonHealthy},
		{name: "free space below twice the reserve", reply: libapi.Library{Books: books, FreeBytes: 10<<30 - 1}, reason: v1alpha1.ReasonLowSpace},
		{name: "free space at twice the reserve", reply: libapi.Library{Books: books, FreeBytes: 10 << 30}, reason: v1alpha1.ReasonHealthy},
		{name: "reload unconfirmed outranks low space", reply: libapi.Library{Books: books, FreeBytes: 1, ReloadUnconfirmed: true}, reason: v1alpha1.ReasonReloadUnconfirmed},
		{name: "unmanaged file errors", reply: libapi.Library{Books: books, FreeBytes: 100 << 30, Unmanaged: []libapi.UnmanagedFile{odd, broken}}, reason: v1alpha1.ReasonUnmanagedFileErrors},
		{name: "low space outranks unmanaged file errors", reply: libapi.Library{Books: books, FreeBytes: 1, Unmanaged: []libapi.UnmanagedFile{broken}}, reason: v1alpha1.ReasonLowSpace},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, testLib())
			h.reconcile(t, "wiki")
			h.markReady(t, "wiki")
			h.librarian.reply = tt.reply

			h.reconcile(t, "wiki")

			lib := h.library(t, "wiki")
			assertCondition(t, lib, v1alpha1.ConditionAvailable, metav1.ConditionTrue, v1alpha1.ReasonServing)
			wantDegraded := metav1.ConditionTrue
			if tt.reason == v1alpha1.ReasonHealthy {
				wantDegraded = metav1.ConditionFalse
			}
			assertCondition(t, lib, v1alpha1.ConditionDegraded, wantDegraded, tt.reason)
			assert.Equal(t, int32(2), lib.Status.Books)
			assert.Equal(t, tt.reply.FreeBytes, lib.Status.FreeBytes)
			var wantFiles []v1alpha1.UnmanagedFile
			for _, u := range tt.reply.Unmanaged {
				wantFiles = append(wantFiles, v1alpha1.UnmanagedFile{Name: u.Name, Error: u.Error})
			}
			assert.Equal(t, wantFiles, lib.Status.UnmanagedFiles)
			assert.Equal(t, "http://wiki-librarian.kiwix.svc:8090", h.librarian.baseURL)
			assert.Equal(t, h.token(t, "wiki"), h.librarian.token)
		})
	}
}

func TestReconcileLibrarianUnreachable(t *testing.T) {
	h := newHarness(t, testLib())
	h.reconcile(t, "wiki")
	h.markReady(t, "wiki")
	h.librarian.err = errors.New("connection refused")

	res := h.reconcile(t, "wiki")

	assert.Equal(t, time.Minute, res.RequeueAfter)
	lib := h.library(t, "wiki")
	assertCondition(t, lib, v1alpha1.ConditionAvailable, metav1.ConditionFalse, v1alpha1.ReasonLibrarianUnreachable)
	assert.Contains(t, meta.FindStatusCondition(lib.Status.Conditions, v1alpha1.ConditionAvailable).Message, "connection refused")
}

func TestSecretNotRotated(t *testing.T) {
	h := newHarness(t, testLib())
	h.reconcile(t, "wiki")
	first := h.token(t, "wiki")

	h.reconcile(t, "wiki")

	assert.Equal(t, first, h.token(t, "wiki"))
}

func TestStatusUpdatedOnlyOnChange(t *testing.T) {
	h := newHarness(t, testLib())

	h.reconcile(t, "wiki")
	h.reconcile(t, "wiki")

	assert.Equal(t, 1, h.statusUpdates)
}

func TestReconcileMissingLibrary(t *testing.T) {
	h := newHarness(t)

	res := h.reconcile(t, "gone")

	assert.Equal(t, ctrl.Result{}, res)
}

func TestUpdateLeavesUnmanagedFields(t *testing.T) {
	created := metav1.NewTime(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "wiki", Namespace: "kiwix", CreationTimestamp: created},
		Spec:       corev1.ServiceSpec{ClusterIP: "10.96.0.10", ClusterIPs: []string{"10.96.0.10"}},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "wiki-data", Namespace: "kiwix", CreationTimestamp: created},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")}},
		},
	}
	h := newHarness(t, testLib(), svc, pvc)

	h.reconcile(t, "wiki")

	var gotSvc corev1.Service
	h.get(t, "wiki", &gotSvc)
	assert.Equal(t, "10.96.0.10", gotSvc.Spec.ClusterIP)
	assert.Len(t, gotSvc.Spec.Ports, 1)
	var gotPVC corev1.PersistentVolumeClaim
	h.get(t, "wiki-data", &gotPVC)
	assert.Equal(t, resource.MustParse("20Gi"), gotPVC.Spec.Resources.Requests[corev1.ResourceStorage])
	assert.Equal(t, "kiwix-operator", gotPVC.Labels["app.kubernetes.io/managed-by"])
	assert.Empty(t, gotPVC.OwnerReferences)
}
