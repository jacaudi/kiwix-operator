// Package libapi holds the wire types of the librarian HTTP API, shared by the librarian, its client and the manager.
package libapi

import "time"

// Attempt kinds: where an attempt's bytes come from.
const (
	KindUpload = "upload"
	KindFetch  = "fetch"
)

// Part names of an upload attempt.
const (
	PartZIM   = "zim"
	PartWARCs = "warcs"
)

// Attempt states, persisted in the librarian manifest.
const (
	StateRegistered = "registered"
	StateReceiving  = "receiving"
	StateStaged     = "staged"
	StatePromoting  = "promoting"
	StatePromoted   = "promoted"
	StateDiscarded  = "discarded"
	StateFailed     = "failed"
)

// Error codes carried in ErrorBody.Code and AttemptState.Code.
const (
	CodeInvalid           = "Invalid"
	CodeUnauthorized      = "Unauthorized"
	CodeNotFound          = "NotFound"
	CodeConflict          = "Conflict"
	CodeBusy              = "Busy"
	CodeCurrentChanged    = "CurrentChanged"
	CodeDuplicateBook     = "DuplicateBook"
	CodeInsufficientSpace = "InsufficientSpace"
	CodeHashMismatch      = "HashMismatch"
	CodeInvalidZim        = "InvalidZim"
	CodeAdoptFileMissing  = "AdoptFileMissing"
	CodeAdoptFileInvalid  = "AdoptFileInvalid"
	CodeInternal          = "Internal"
)

// RegisterRequest registers an attempt with the librarian. TokenHash is the lowercase hex SHA-256 of the attempt's upload token.
type RegisterRequest struct {
	Archive               string     `json:"archive"`
	Attempt               string     `json:"attempt"`
	Kind                  string     `json:"kind"`
	TokenHash             string     `json:"tokenHash"`
	Parts                 []string   `json:"parts,omitempty"`
	Fetch                 *FetchSpec `json:"fetch,omitempty"`
	ExpectedCurrentBookID string     `json:"expectedCurrentBookID,omitempty"`
	Deadline              time.Time  `json:"deadline,omitzero"`
}

// FetchSpec is the URL a fetch attempt pulls, with the size and SHA-256 the source declares when it declares them.
type FetchSpec struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

// AttemptState is the librarian's report on one attempt.
type AttemptState struct {
	Archive string    `json:"archive"`
	Attempt string    `json:"attempt"`
	Kind    string    `json:"kind"`
	State   string    `json:"state"`
	Code    string    `json:"code,omitempty"`
	Error   string    `json:"error,omitempty"`
	Staged  *BookInfo `json:"staged,omitempty"`
	Current *BookInfo `json:"current,omitempty"`
}

// PromoteRequest asks the librarian to make a staged book current if the current book is still the expected one.
type PromoteRequest struct {
	ExpectedCurrentBookID string `json:"expectedCurrentBookID,omitempty"`
	SourceVersion         string `json:"sourceVersion"`
	SourceFingerprint     string `json:"sourceFingerprint"`
}

// AdoptRequest asks the librarian to adopt an existing unmanaged file as an archive's current book.
type AdoptRequest struct {
	File                  string `json:"file"`
	ExpectedCurrentBookID string `json:"expectedCurrentBookID,omitempty"`
}

// BookInfo describes a book. Archive is empty for an unmanaged book.
type BookInfo struct {
	Archive           string `json:"archive,omitempty"`
	File              string `json:"file"`
	BookID            string `json:"bookID"`
	ArticleCount      int64  `json:"articleCount"`
	SizeKiB           int64  `json:"sizeKiB"`
	SHA256            string `json:"sha256,omitempty"`
	SourceVersion     string `json:"sourceVersion,omitempty"`
	SourceFingerprint string `json:"sourceFingerprint,omitempty"`
}

// Library summarizes what the librarian serves.
type Library struct {
	Books             []BookInfo      `json:"books"`
	Unmanaged         []UnmanagedFile `json:"unmanaged"`
	FreeBytes         int64           `json:"freeBytes"`
	ReloadUnconfirmed bool            `json:"reloadUnconfirmed"`
}

// UnmanagedFile is a top-level .zim file the librarian could not serve, with the reason.
type UnmanagedFile struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

// ErrorBody is the JSON body of every non-2xx librarian response.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
