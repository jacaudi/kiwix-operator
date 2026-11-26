package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DeletionPolicy says what happens to the ZIM file when the archive is deleted
// +kubebuilder:validation:Enum=Retain;Delete
type DeletionPolicy string

const (
	// DeletionPolicyRetain keeps the file on the volume
	DeletionPolicyRetain DeletionPolicy = "Retain"
	// DeletionPolicyDelete removes the file from the volume
	DeletionPolicyDelete DeletionPolicy = "Delete"
)

// AttemptResult is the outcome of a finished attempt
type AttemptResult string

const (
	// AttemptResultSucceeded means the new version was promoted
	AttemptResultSucceeded AttemptResult = "Succeeded"
	// AttemptResultRejected means the health gate refused the new version
	AttemptResultRejected AttemptResult = "Rejected"
	// AttemptResultFailed means the attempt ended in an error
	AttemptResultFailed AttemptResult = "Failed"
	// AttemptResultUnchanged means the source had nothing newer
	AttemptResultUnchanged AttemptResult = "Unchanged"
)

// ZimArchiveSpec defines the desired state of ZimArchive
type ZimArchiveSpec struct {
	// LibraryRef names the KiwixLibrary in the same namespace; it cannot change after creation
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="libraryRef is immutable"
	// +kubebuilder:validation:XValidation:rule="self.name.size() > 0",message="libraryRef.name must not be empty"
	LibraryRef corev1.LocalObjectReference `json:"libraryRef"`

	// Source says where the ZIM comes from
	Source SourceSpec `json:"source"`

	// Refresh configures scheduled refreshes and the health gate
	// +kubebuilder:default={}
	// +optional
	Refresh RefreshSpec `json:"refresh"`

	// AdoptFile names an unmanaged file in the library to adopt as the first version
	// +optional
	AdoptFile string `json:"adoptFile,omitempty"`

	// Notify configures notifications for this archive
	// +kubebuilder:default={}
	// +optional
	Notify ArchiveNotifySpec `json:"notify,omitzero"`

	// DeletionPolicy says what happens to the file when the archive is deleted
	// +kubebuilder:default=Retain
	// +optional
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

// SourceSpec is a crawl or a download
// +kubebuilder:validation:XValidation:rule="has(self.crawl) != has(self.download)",message="exactly one of crawl or download must be set"
type SourceSpec struct {
	// Crawl builds the ZIM from a website
	// +optional
	Crawl *CrawlSource `json:"crawl,omitempty"`

	// Download fetches a prebuilt ZIM
	// +optional
	Download *DownloadSource `json:"download,omitempty"`
}

// CrawlSource builds a ZIM by crawling a website with zimit
type CrawlSource struct {
	// Seeds are the URLs the crawl starts from
	// +kubebuilder:validation:MinItems=1
	Seeds []string `json:"seeds"`

	// Sitemap is a sitemap URL to seed the crawl from
	// +optional
	Sitemap string `json:"sitemap,omitempty"`

	// ScopeExcludeRx is a regular expression for URLs the crawl must skip
	// +optional
	ScopeExcludeRx string `json:"scopeExcludeRx,omitempty"`

	// Workers is the number of parallel crawl workers
	// +kubebuilder:default=4
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=16
	// +optional
	Workers int32 `json:"workers,omitzero"`

	// Title is the ZIM title
	// +optional
	Title string `json:"title,omitempty"`

	// Timeout is how long the crawl Job may run
	// +kubebuilder:default="48h"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ms|s|m|h))+$`
	// +optional
	Timeout metav1.Duration `json:"timeout,omitzero"`

	// KeepWarcs keeps the WARC files after the crawl
	// +optional
	KeepWarcs bool `json:"keepWarcs,omitzero"`

	// CustomCSS is CSS injected into the ZIM
	// +optional
	CustomCSS string `json:"customCss,omitempty"`

	// Scratch is the temporary volume the crawl writes to
	// +kubebuilder:default={}
	// +optional
	Scratch ScratchSpec `json:"scratch,omitzero"`

	// Resources are the crawl container's resource requirements
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitzero"`
}

// ScratchSpec is the temporary volume for a crawl
type ScratchSpec struct {
	// Size is the capacity of the scratch volume
	// +kubebuilder:default="20Gi"
	// +optional
	Size resource.Quantity `json:"size,omitzero"`

	// StorageClassName is the class of the scratch volume
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// DownloadSource fetches a prebuilt ZIM from the Kiwix catalog or a URL
// +kubebuilder:validation:XValidation:rule="has(self.catalog) != has(self.url)",message="exactly one of catalog or url must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.sha256) || has(self.url)",message="sha256 is only valid with url"
type DownloadSource struct {
	// Catalog selects a book from the Kiwix catalog
	// +optional
	Catalog *CatalogRef `json:"catalog,omitempty"`

	// URL is a direct link to a ZIM file
	// +kubebuilder:validation:MinLength=1
	// +optional
	URL string `json:"url,omitempty"`

	// SHA256 is the expected digest of the file at URL
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{64}$`
	// +optional
	SHA256 string `json:"sha256,omitempty"`
}

// CatalogRef selects a book from the Kiwix catalog
type CatalogRef struct {
	// Name is the catalog book name
	Name string `json:"name"`

	// Flavour is the catalog flavour, such as maxi or nopic
	// +optional
	Flavour string `json:"flavour,omitempty"`
}

// RefreshSpec configures scheduled refreshes and the health gate
type RefreshSpec struct {
	// Schedule is a cron expression; empty means no scheduled refresh
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// TimeZone is the IANA time zone the schedule is evaluated in
	// +optional
	TimeZone string `json:"timeZone,omitempty"`

	// MinArticleRatio is the smallest article count, as a percent of the current version, a refresh may have
	// +kubebuilder:default=80
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	MinArticleRatio int32 `json:"minArticleRatio"`

	// MinSizeRatio is the smallest file size, as a percent of the current version, a refresh may have
	// +kubebuilder:default=80
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	MinSizeRatio int32 `json:"minSizeRatio"`
}

// ArchiveNotifySpec configures notifications for one archive
type ArchiveNotifySpec struct {
	// OnSuccess also notifies when a refresh succeeds
	// +optional
	OnSuccess bool `json:"onSuccess,omitzero"`
}

// ZimArchiveStatus defines the observed state of ZimArchive
type ZimArchiveStatus struct {
	// ObservedGeneration is the generation the status describes
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitzero"`

	// Conditions represent the latest available observations
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Current is the version the library serves
	// +optional
	Current *CurrentVersion `json:"current,omitempty"`

	// LastAttempt is the open or most recently finished attempt
	// +optional
	LastAttempt *AttemptStatus `json:"lastAttempt,omitempty"`

	// FailedAttempts counts consecutive failed attempts
	// +optional
	FailedAttempts int32 `json:"failedAttempts,omitzero"`

	// LastFailureNotifiedAt is when the last failure notification went out
	// +optional
	LastFailureNotifiedAt *metav1.Time `json:"lastFailureNotifiedAt,omitempty"`

	// LastHandledRequestedAt is the requestedAt annotation value last acted on
	// +optional
	LastHandledRequestedAt string `json:"lastHandledRequestedAt,omitempty"`

	// LastScheduledAt is the last schedule tick acted on
	// +optional
	LastScheduledAt *metav1.Time `json:"lastScheduledAt,omitempty"`

	// NextRefreshAt is when the next refresh is due
	// +optional
	NextRefreshAt *metav1.Time `json:"nextRefreshAt,omitempty"`

	// FinalizerFirstFailureAt is when deletion first failed or stayed pending
	// +optional
	FinalizerFirstFailureAt *metav1.Time `json:"finalizerFirstFailureAt,omitempty"`

	// AdoptMissingNotified records that the missing adoptFile notification went out
	// +optional
	AdoptMissingNotified bool `json:"adoptMissingNotified,omitzero"`
}

// CurrentVersion is the version of the archive the library serves
type CurrentVersion struct {
	// File is the ZIM file name
	File string `json:"file"`

	// BookID is the ZIM UUID in dashed lowercase form
	BookID string `json:"bookId"`

	// ArticleCount is the number of articles in the file
	// +optional
	ArticleCount int64 `json:"articleCount,omitzero"`

	// SizeKiB is the file size in KiB
	// +optional
	SizeKiB int64 `json:"sizeKiB,omitzero"`

	// SHA256 is the digest of the file
	// +optional
	SHA256 string `json:"sha256,omitempty"`

	// SourceVersion is the source's version as YYYY-MM
	// +optional
	SourceVersion string `json:"sourceVersion,omitempty"`

	// SourceFingerprint identifies where the version came from
	// +optional
	SourceFingerprint string `json:"sourceFingerprint,omitempty"`

	// PromotedAt is when the file became current
	// +optional
	PromotedAt *metav1.Time `json:"promotedAt,omitempty"`
}

// AttemptStatus records one crawl, upload or fetch attempt
type AttemptStatus struct {
	// ID is the attempt ID; empty when the attempt ended Unchanged
	// +optional
	ID string `json:"id,omitempty"`

	// Kind is "upload" for a crawl's result or "fetch" for a download
	// +optional
	Kind string `json:"kind,omitempty"`

	// JobCreated records that the crawl Job exists
	// +optional
	JobCreated bool `json:"jobCreated,omitzero"`

	// Trigger is "initial", "schedule", "requested" or "backoff"
	// +optional
	Trigger string `json:"trigger,omitempty"`

	// RequestedAt is the requestedAt annotation value that started the attempt
	// +optional
	RequestedAt string `json:"requestedAt,omitempty"`

	// ScheduleTick is the schedule tick that started the attempt
	// +optional
	ScheduleTick *metav1.Time `json:"scheduleTick,omitempty"`

	// StartedAt is when the attempt began
	StartedAt metav1.Time `json:"startedAt"`

	// FinishedAt is when the attempt ended; nil while it is open
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// Deadline is when the attempt is abandoned
	// +optional
	Deadline *metav1.Time `json:"deadline,omitempty"`

	// SourceVersion is the catalog version a catalog fetch downloads, as YYYY-MM
	// +optional
	SourceVersion string `json:"sourceVersion,omitempty"`

	// Result is the outcome once the attempt has ended
	// +optional
	Result AttemptResult `json:"result,omitempty"`

	// Reason is the condition reason for a failed or rejected attempt
	// +optional
	Reason string `json:"reason,omitempty"`

	// Message describes the outcome
	// +optional
	Message string `json:"message,omitempty"`

	// Notified records that this attempt's notification went out
	// +optional
	Notified bool `json:"notified,omitzero"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 45",message="name must be at most 45 characters"
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="name must be a DNS label"
// +kubebuilder:printcolumn:name="Library",type=string,JSONPath=`.spec.libraryRef.name`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.current.sourceVersion`
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=`.status.conditions[?(@.type=="Available")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ZimArchive is the Schema for the zimarchives API
type ZimArchive struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec   ZimArchiveSpec   `json:"spec"`
	Status ZimArchiveStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ZimArchiveList contains a list of ZimArchive
type ZimArchiveList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ZimArchive `json:"items"`
}
