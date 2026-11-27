package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// KiwixLibrarySpec defines the desired state of KiwixLibrary
type KiwixLibrarySpec struct {
	// Storage is the volume holding the ZIM files; it cannot change after creation
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="storage is immutable"
	Storage StorageSpec `json:"storage"`

	// Server configures the kiwix-serve pod
	// +optional
	Server ServerSpec `json:"server,omitzero"`

	// Route exposes the library through a Gateway API HTTPRoute
	// +optional
	Route *RouteSpec `json:"route,omitempty"`

	// Downloads limits concurrent catalog and URL downloads
	// +kubebuilder:default={}
	// +optional
	Downloads ConcurrencySpec `json:"downloads,omitzero"`

	// Crawls limits concurrent crawl jobs
	// +kubebuilder:default={}
	// +optional
	Crawls ConcurrencySpec `json:"crawls,omitzero"`

	// ReserveSpace is the free space a write must leave on the volume
	// +kubebuilder:default="5Gi"
	// +optional
	ReserveSpace resource.Quantity `json:"reserveSpace,omitzero"`

	// Notify configures library-wide notifications
	// +optional
	Notify *NotifySpec `json:"notify,omitempty"`
}

// StorageSpec selects an existing claim or asks the operator to create one
// +kubebuilder:validation:XValidation:rule="has(self.existingClaim) != has(self.size)",message="exactly one of existingClaim or size must be set"
type StorageSpec struct {
	// ExistingClaim names a PersistentVolumeClaim in the same namespace
	// +kubebuilder:validation:MinLength=1
	// +optional
	ExistingClaim string `json:"existingClaim,omitempty"`

	// Size is the capacity of the claim the operator creates
	// +optional
	Size *resource.Quantity `json:"size,omitempty"`

	// StorageClassName is the class of the claim the operator creates
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// ServerSpec configures the kiwix-serve pod
type ServerSpec struct {
	// BlockExternalLinks stops kiwix-serve from linking to external sites
	// +optional
	BlockExternalLinks bool `json:"blockExternalLinks,omitzero"`

	// Resources are the kiwix-serve container's resource requirements
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitzero"`

	// IngressFrom lists the peers allowed to reach kiwix-serve
	// +optional
	IngressFrom []networkingv1.NetworkPolicyPeer `json:"ingressFrom,omitempty"`
}

// RouteSpec exposes the library through a Gateway API HTTPRoute
type RouteSpec struct {
	// ParentRefs are the Gateways the route attaches to
	ParentRefs []gwapiv1.ParentReference `json:"parentRefs"`

	// Hostnames are the hosts the route matches
	// +optional
	Hostnames []gwapiv1.Hostname `json:"hostnames,omitempty"`
}

// ConcurrencySpec limits how many jobs of one kind run at once
type ConcurrencySpec struct {
	// MaxConcurrent is the number of jobs allowed to run at once
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxConcurrent int32 `json:"maxConcurrent,omitzero"`
}

// NotifySpec configures notifications
type NotifySpec struct {
	// Webhook receives a JSON notification
	Webhook WebhookSpec `json:"webhook"`
}

// WebhookSpec is a webhook endpoint
type WebhookSpec struct {
	// URL is the endpoint that receives the notification
	URL string `json:"url"`
}

// KiwixLibraryStatus defines the observed state of KiwixLibrary
type KiwixLibraryStatus struct {
	// ObservedGeneration is the generation the status describes
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitzero"`

	// Conditions represent the latest available observations
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Books is the number of books served
	// +optional
	Books int32 `json:"books,omitzero"`

	// FreeBytes is the free space on the volume
	// +optional
	FreeBytes int64 `json:"freeBytes,omitzero"`

	// UnmanagedFiles lists files that could not be served
	// +optional
	UnmanagedFiles []UnmanagedFile `json:"unmanagedFiles,omitempty"`
}

// UnmanagedFile is a ZIM file the librarian could not serve
type UnmanagedFile struct {
	// Name is the file name
	Name string `json:"name"`

	// Error is why the file was rejected
	Error string `json:"error"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// KiwixLibrary is the Schema for the kiwixlibraries API
type KiwixLibrary struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec   KiwixLibrarySpec   `json:"spec"`
	Status KiwixLibraryStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KiwixLibraryList contains a list of KiwixLibrary
type KiwixLibraryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KiwixLibrary `json:"items"`
}
