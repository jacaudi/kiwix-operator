package controller

import (
	"fmt"
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/jacaudi/kiwix-operator/api/v1alpha1"
)

// Ports of the kiwix pod's two containers.
const (
	servePort     = 8080
	librarianPort = 8090
)

const (
	dataDir         = "/data"
	adminTokenKey   = "token"
	adminTokenEnv   = "KIWIX_LIBRARIAN_ADMIN_TOKEN"
	kiwixUID        = 1000
	labelName       = "app.kubernetes.io/name"
	labelInstance   = "app.kubernetes.io/instance"
	kiwixAppName    = "kiwix"
	operatorAppName = "kiwix-operator"
)

func adminSecretName(lib *v1alpha1.KiwixLibrary) string  { return lib.Name + "-librarian-admin" }
func librarianSvcName(lib *v1alpha1.KiwixLibrary) string { return lib.Name + "-librarian" }
func dataClaimName(lib *v1alpha1.KiwixLibrary) string    { return lib.Name + "-data" }

// claimName is the PVC a library's pod mounts: its existing claim, or the one the operator creates.
func claimName(lib *v1alpha1.KiwixLibrary) string {
	if lib.Spec.Storage.ExistingClaim != "" {
		return lib.Spec.Storage.ExistingClaim
	}
	return dataClaimName(lib)
}

// librarianURL is the in-cluster base URL of a library's librarian API.
func librarianURL(lib *v1alpha1.KiwixLibrary) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", librarianSvcName(lib), lib.Namespace, librarianPort)
}

// podSelector selects a library's kiwix pod.
func podSelector(lib *v1alpha1.KiwixLibrary) map[string]string {
	return map[string]string{labelName: kiwixAppName, labelInstance: lib.Name}
}

// podLabels are a library's kiwix pod labels, also put on every object the operator creates for the library.
func podLabels(lib *v1alpha1.KiwixLibrary) map[string]string {
	l := podSelector(lib)
	l[v1alpha1.LabelManagedBy] = v1alpha1.ManagedByValue
	return l
}

func objectMeta(lib *v1alpha1.KiwixLibrary, name string, labels map[string]string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: lib.Namespace, Labels: maps.Clone(labels)}
}

func httpProbe(path string, port int32) *corev1.Probe {
	return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromInt32(port)}}}
}

func lockedDown() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: new(false),
		ReadOnlyRootFilesystem:   new(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// buildDeployment is a library's kiwix pod: the librarian as a native sidecar and kiwix-serve, both from image, mounting claim at /data.
func buildDeployment(lib *v1alpha1.KiwixLibrary, image, claim string) *appsv1.Deployment {
	serveArgs := []string{"--library", dataDir + "/library.xml", "--monitorLibrary", "--port", fmt.Sprint(servePort)}
	if lib.Spec.Server.BlockExternalLinks {
		serveArgs = append(serveArgs, "--blockexternal")
	}
	startup := httpProbe("/healthz/startup", librarianPort)
	startup.PeriodSeconds = 5
	// 5 s x 360 allows the librarian 30 minutes to start.
	startup.FailureThreshold = 360
	tmp := corev1.VolumeMount{Name: "tmp", MountPath: "/tmp"}

	librarian := corev1.Container{
		Name:          "librarian",
		Image:         image,
		RestartPolicy: new(corev1.ContainerRestartPolicyAlways),
		Args: []string{
			"librarian",
			"--data-dir=" + dataDir,
			fmt.Sprintf("--listen=:%d", librarianPort),
			fmt.Sprintf("--kiwix-serve-url=http://127.0.0.1:%d", servePort),
			fmt.Sprintf("--reserve-space=%d", lib.Spec.ReserveSpace.Value()),
			fmt.Sprintf("--max-fetches=%d", lib.Spec.Downloads.MaxConcurrent),
			"--kiwix-manage=/usr/local/bin/kiwix-manage",
		},
		Env: []corev1.EnvVar{{
			Name: adminTokenEnv,
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: adminSecretName(lib)},
				Key:                  adminTokenKey,
			}},
		}},
		Ports:           []corev1.ContainerPort{{Name: "librarian", ContainerPort: librarianPort, Protocol: corev1.ProtocolTCP}},
		StartupProbe:    startup,
		LivenessProbe:   httpProbe("/healthz/live", librarianPort),
		VolumeMounts:    []corev1.VolumeMount{{Name: "data", MountPath: dataDir}, tmp},
		SecurityContext: lockedDown(),
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
		},
	}
	serve := corev1.Container{
		Name:            "kiwix-serve",
		Image:           image,
		Command:         []string{"/usr/local/bin/kiwix-serve"},
		Args:            serveArgs,
		Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: servePort, Protocol: corev1.ProtocolTCP}},
		ReadinessProbe:  httpProbe("/catalog/search?count=1", servePort),
		VolumeMounts:    []corev1.VolumeMount{{Name: "data", MountPath: dataDir, ReadOnly: true}, tmp},
		SecurityContext: lockedDown(),
		Resources:       lib.Spec.Server.Resources,
	}

	labels := podLabels(lib)
	return &appsv1.Deployment{
		ObjectMeta: objectMeta(lib, lib.Name, labels),
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: podSelector(lib)},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: maps.Clone(labels)},
				Spec: corev1.PodSpec{
					InitContainers:                []corev1.Container{librarian},
					Containers:                    []corev1.Container{serve},
					AutomountServiceAccountToken:  new(false),
					TerminationGracePeriodSeconds: new(int64(10)),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:        new(true),
						RunAsUser:           new(int64(kiwixUID)),
						RunAsGroup:          new(int64(kiwixUID)),
						FSGroup:             new(int64(kiwixUID)),
						FSGroupChangePolicy: new(corev1.FSGroupChangeOnRootMismatch),
						SeccompProfile:      &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Volumes: []corev1.Volume{
						{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
}

// buildServices returns kiwix-serve's Service and the librarian's, which also lists the pod while kiwix-serve is not ready.
func buildServices(lib *v1alpha1.KiwixLibrary) (serve, librarian *corev1.Service) {
	svc := func(name, port string, number int32, notReady bool) *corev1.Service {
		return &corev1.Service{
			ObjectMeta: objectMeta(lib, name, podLabels(lib)),
			Spec: corev1.ServiceSpec{
				Selector:                 podSelector(lib),
				Ports:                    []corev1.ServicePort{{Name: port, Protocol: corev1.ProtocolTCP, Port: number, TargetPort: intstr.FromInt32(number)}},
				PublishNotReadyAddresses: notReady,
			},
		}
	}
	return svc(lib.Name, "http", servePort, false), svc(librarianSvcName(lib), "librarian", librarianPort, true)
}

// buildHTTPRoute routes spec.route to kiwix-serve's Service; Gateway API takes a port number, not a name.
func buildHTTPRoute(lib *v1alpha1.KiwixLibrary) *gwapiv1.HTTPRoute {
	ref := gwapiv1.HTTPBackendRef{BackendRef: gwapiv1.BackendRef{BackendObjectReference: gwapiv1.BackendObjectReference{
		Name: gwapiv1.ObjectName(lib.Name),
		Port: new(gwapiv1.PortNumber(servePort)),
	}}}
	return &gwapiv1.HTTPRoute{
		ObjectMeta: objectMeta(lib, lib.Name, podLabels(lib)),
		Spec: gwapiv1.HTTPRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{ParentRefs: lib.Spec.Route.ParentRefs},
			Hostnames:       lib.Spec.Route.Hostnames,
			Rules:           []gwapiv1.HTTPRouteRule{{BackendRefs: []gwapiv1.HTTPBackendRef{ref}}},
		},
	}
}
