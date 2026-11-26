// Package librarian holds the librarian's state: the manifest of books and attempts, and the scan that reconciles it with the volume.
package librarian

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/jacaudi/kiwix-operator/internal/libapi"
)

// manifestFile is the manifest's file name inside the librarian state directory.
const manifestFile = "manifest.json"

// Book is one served ZIM. Element is the <book/> element kiwix-manage wrote, with Path rewritten to the absolute file.
type Book struct {
	BookID       string `json:"bookID"`
	Path         string `json:"path"`
	ArticleCount int64  `json:"articleCount"`
	SizeKiB      int64  `json:"sizeKiB"`
	Element      string `json:"element"`
}

// ArchiveCurrent is the current version of one archive.
type ArchiveCurrent struct {
	File              string `json:"file"`
	BookID            string `json:"bookID"`
	ArticleCount      int64  `json:"articleCount"`
	SizeKiB           int64  `json:"sizeKiB"`
	SHA256            string `json:"sha256,omitempty"`
	SourceVersion     string `json:"sourceVersion,omitempty"`
	SourceFingerprint string `json:"sourceFingerprint,omitempty"`
}

// Intent records a promotion or adoption in progress so a restart can roll it forward.
type Intent struct {
	Op                    string `json:"op"`
	Archive               string `json:"archive"`
	Attempt               string `json:"attempt,omitempty"`
	Source                string `json:"source,omitempty"`
	ExpectedCurrentBookID string `json:"expectedCurrentBookID,omitempty"`
	NewBookID             string `json:"newBookID"`
	Book                  Book   `json:"book"`
}

// AttemptRecord is one registered attempt and its progress.
type AttemptRecord struct {
	libapi.RegisterRequest
	State       string                `json:"state"`
	Code        string                `json:"code,omitempty"`
	Error       string                `json:"error,omitempty"`
	Parts       map[string]PartRecord `json:"partStates,omitempty"`
	PartialPath string                `json:"partialPath,omitempty"`
	Staged      *Book                 `json:"staged,omitempty"`
	UpdatedAt   time.Time             `json:"updatedAt,omitzero"`
}

// PartRecord is one received part of an upload attempt.
type PartRecord struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Done   bool   `json:"done"`
}

// Manifest is everything the librarian persists. Books is keyed by file basename and Attempts by attempt ID.
type Manifest struct {
	Books          map[string]Book           `json:"books"`
	Archives       map[string]ArchiveCurrent `json:"archives"`
	Intents        []Intent                  `json:"intents,omitempty"`
	PendingUnlinks []string                  `json:"pendingUnlinks,omitempty"`
	Attempts       map[string]AttemptRecord  `json:"attempts"`
}

func newManifest() *Manifest {
	return &Manifest{
		Books:    map[string]Book{},
		Archives: map[string]ArchiveCurrent{},
		Attempts: map[string]AttemptRecord{},
	}
}

// LoadManifest reads the manifest from dir. With no manifest file it returns an empty manifest and existed=false.
func LoadManifest(dir string) (m *Manifest, existed bool, err error) {
	path := filepath.Join(dir, manifestFile)
	data, err := os.ReadFile(path) //nolint:gosec // dir is the librarian state directory from its flags
	if errors.Is(err, fs.ErrNotExist) {
		return newManifest(), false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read manifest %s: %w", path, err)
	}
	m = newManifest()
	if err := json.Unmarshal(data, m); err != nil {
		return nil, false, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	return m, true, nil
}

// Save writes the manifest into dir atomically, creating dir on first use.
func (m *Manifest) Save(dir string) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create manifest directory %s: %w", dir, err)
	}
	return WriteFileAtomic(filepath.Join(dir, manifestFile), data, nil)
}
