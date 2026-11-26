package librarian

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jacaudi/kiwix-operator/internal/libapi"
)

func TestManifestRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".librarian")

	got, existed, err := LoadManifest(dir)
	require.NoError(t, err)
	assert.False(t, existed)
	assert.NotNil(t, got.Books)
	assert.NotNil(t, got.Archives)
	assert.NotNil(t, got.Attempts)
	assert.Empty(t, got.Books)
	assert.Empty(t, got.Archives)
	assert.Empty(t, got.Attempts)

	book := Book{BookID: uuidA, Path: "/data/a.zim", ArticleCount: 3, SizeKiB: 12, Element: `<book id="a"/>`}
	want := &Manifest{
		Books: map[string]Book{"a.zim": book},
		Archives: map[string]ArchiveCurrent{
			"a": {File: "a.zim", BookID: uuidA, ArticleCount: 3, SizeKiB: 12, SHA256: "abc", SourceVersion: "2026-11", SourceFingerprint: "url:u"},
		},
		Intents:        []Intent{{Op: "promote", Archive: "a", Attempt: "abcdefgh", Source: "stage", ExpectedCurrentBookID: uuidB, NewBookID: uuidA, Book: book}},
		PendingUnlinks: []string{"old.zim"},
		Attempts: map[string]AttemptRecord{
			"abcdefgh": {
				RegisterRequest: libapi.RegisterRequest{
					Archive: "a", Attempt: "abcdefgh", Kind: libapi.KindFetch, TokenHash: "h",
					Fetch:    &libapi.FetchSpec{URL: "https://example.test/a.zim", SHA256: "abc", Size: 9},
					Deadline: time.Date(2026, 11, 25, 1, 2, 3, 0, time.UTC),
				},
				State:       libapi.StateStaged,
				Parts:       map[string]PartRecord{"0": {SHA256: "abc", Size: 9, Done: true}},
				PartialPath: "/data/.librarian/staging/abcdefgh",
				Staged:      &book,
				UpdatedAt:   time.Date(2026, 11, 25, 4, 5, 6, 0, time.UTC),
			},
			"ijklmnop": {
				RegisterRequest: libapi.RegisterRequest{Archive: "a", Attempt: "ijklmnop", Kind: libapi.KindUpload, TokenHash: "h", Parts: []string{"zim", "warcs"}},
				State:           libapi.StateReceiving,
				Parts:           map[string]PartRecord{"zim": {SHA256: "def", Size: 5, Done: true}, "warcs": {}},
			},
		},
	}
	require.NoError(t, want.Save(dir))

	got, existed, err = LoadManifest(dir)
	require.NoError(t, err)
	assert.True(t, existed)
	assert.Equal(t, want, got)
}

func TestLoadManifestCorrupt(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, manifestFile), []byte("{not json"), 0o600))

	_, _, err := LoadManifest(dir)
	require.Error(t, err)
}

func TestWriteFileAtomicSetsMtime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "library.xml")
	mtime := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)

	require.NoError(t, WriteFileAtomic(path, []byte("<library/>"), &mtime))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, info.ModTime().Equal(mtime), "mtime %v, want %v", info.ModTime(), mtime)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "<library/>", string(data))
	assert.Equal(t, []string{"library.xml"}, dirNames(t, dir))
}

func TestWriteFileAtomicLeavesNoTempOnError(t *testing.T) {
	dir := t.TempDir()
	// A non-empty directory at the destination makes the rename fail after the temp file is written.
	target := filepath.Join(dir, "library.xml")
	require.NoError(t, os.MkdirAll(filepath.Join(target, "sub"), 0o750))

	require.Error(t, WriteFileAtomic(target, []byte("x"), nil))

	assert.Equal(t, []string{"library.xml"}, dirNames(t, dir))
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
