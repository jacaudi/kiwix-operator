package v1alpha1

// Condition types shared by KiwixLibrary and ZimArchive.
const (
	ConditionAvailable   = "Available"
	ConditionProgressing = "Progressing"
	ConditionDegraded    = "Degraded"
)

// Reasons for the Available condition.
const (
	ReasonServed               = "Served"
	ReasonNotYetFetched        = "NotYetFetched"
	ReasonAwaitingAdoption     = "AwaitingAdoption"
	ReasonLibraryUnavailable   = "LibraryUnavailable"
	ReasonServing              = "Serving"
	ReasonStarting             = "Starting"
	ReasonLibrarianUnreachable = "LibrarianUnreachable"
)

// Reasons for the Progressing condition.
const (
	ReasonCrawling    = "Crawling"
	ReasonUploading   = "Uploading"
	ReasonDownloading = "Downloading"
	ReasonPromoting   = "Promoting"
	ReasonQueued      = "Queued"
	ReasonIdle        = "Idle"
)

// Reasons for the Degraded condition.
const (
	ReasonHealthy             = "Healthy"
	ReasonCrawlFailed         = "CrawlFailed"
	ReasonUploadFailed        = "UploadFailed"
	ReasonDownloadFailed      = "DownloadFailed"
	ReasonHashMismatch        = "HashMismatch"
	ReasonGateRejected        = "GateRejected"
	ReasonInsufficientSpace   = "InsufficientSpace"
	ReasonCatalogLookupFailed = "CatalogLookupFailed"
	ReasonDuplicateBook       = "DuplicateBook"
	ReasonAdoptFileMissing    = "AdoptFileMissing"
	ReasonAdoptFileInvalid    = "AdoptFileInvalid"
	ReasonAdoptConflict       = "AdoptConflict"
	ReasonCurrentLost         = "CurrentLost"
	ReasonInvalidSchedule     = "InvalidSchedule"
	ReasonLowSpace            = "LowSpace"
	ReasonUnmanagedFileErrors = "UnmanagedFileErrors"
	ReasonReloadUnconfirmed   = "ReloadUnconfirmed"
	ReasonRouteUnsupported    = "RouteUnsupported"
	ReasonClaimInUse          = "ClaimInUse"
)

// Annotation, finalizer and label keys the operator owns.
const (
	AnnotationRequestedAt = "kiwix.jacaudi.dev/requestedAt"
	FinalizerName         = "kiwix.jacaudi.dev/finalizer"
	LabelLibrary          = "kiwix.jacaudi.dev/library"
	LabelArchive          = "kiwix.jacaudi.dev/archive"
	LabelAttempt          = "kiwix.jacaudi.dev/attempt"
	LabelManagedBy        = "app.kubernetes.io/managed-by"
	ManagedByValue        = "kiwix-operator"
)
