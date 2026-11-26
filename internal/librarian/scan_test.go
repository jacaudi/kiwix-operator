package librarian

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jacaudi/kiwix-operator/internal/libapi"
	"github.com/jacaudi/kiwix-operator/internal/zim"
)

const (
	fixturesDir = "../../test/fixtures/zim"
	uuidA       = "6e34ed96-2e7e-2cc4-ed48-fa927ad74927" // small-a.zim
	uuidB       = "49fc9acf-17ee-c886-d939-d9db2a22162f" // small-b.zim
	uuidTiny    = "59f93c89-7426-d03b-cb25-0bd89d7a2eba" // tiny.zim
)

// fakeReader stands in for kiwix-manage: it reports the header UUID unless told otherwise.
type fakeReader struct {
	fail     map[string]error
	override map[string]Book
	calls    []string
}

func newFakeReader() *fakeReader {
	return &fakeReader{fail: map[string]error{}, override: map[string]Book{}}
}

func (f *fakeReader) Read(_ context.Context, zimPath string) (Book, error) {
	f.calls = append(f.calls, filepath.Base(zimPath))
	if err := f.fail[zimPath]; err != nil {
		return Book{}, err
	}
	if b, ok := f.override[zimPath]; ok {
		return b, nil
	}
	id, err := zim.ReadUUID(zimPath)
	if err != nil {
		return Book{}, err
	}
	return Book{BookID: id, Path: zimPath, ArticleCount: 3, SizeKiB: 1, Element: `<book id="` + id + `" path="` + zimPath + `" date="2026-10-03"/>`}, nil
}

// place copies a fixture into dir under name and returns its path.
func place(t *testing.T, dir, name, fixture string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixturesDir, fixture))
	require.NoError(t, err)
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

func scan(t *testing.T, dir string, r BookReader, m *Manifest) ([]libapi.UnmanagedFile, bool) {
	t.Helper()
	unmanaged, changed, err := Scan(t.Context(), dir, r, m)
	require.NoError(t, err)
	return unmanaged, changed
}

func emptyManifest() *Manifest { return newManifest() }

func TestScanCanonicalBecomesCurrent(t *testing.T) {
	dir := t.TempDir()
	place(t, dir, "arm-reference.zim", "small-a.zim")
	m := emptyManifest()

	unmanaged, changed := scan(t, dir, newFakeReader(), m)

	assert.Empty(t, unmanaged)
	assert.True(t, changed)
	assert.Equal(t, ArchiveCurrent{File: "arm-reference.zim", BookID: uuidA, ArticleCount: 3, SizeKiB: 1}, m.Archives["arm-reference"])
	assert.Equal(t, uuidA, m.Books["arm-reference.zim"].BookID)
	assert.Equal(t, filepath.Join(dir, "arm-reference.zim"), m.Books["arm-reference.zim"].Path)
}

func TestScanDiskWinsOverManifest(t *testing.T) {
	dir := t.TempDir()
	place(t, dir, "arm-reference.zim", "small-a.zim")
	m := emptyManifest()
	m.Archives["arm-reference"] = ArchiveCurrent{File: "arm-reference.zim", BookID: uuidB, SHA256: "stale", SourceVersion: "2026-01", SourceFingerprint: "url:old"}

	_, changed := scan(t, dir, newFakeReader(), m)

	assert.True(t, changed)
	assert.Equal(t, ArchiveCurrent{File: "arm-reference.zim", BookID: uuidA, ArticleCount: 3, SizeKiB: 1}, m.Archives["arm-reference"])
}

func TestScanKeepsProvenanceWhenUUIDMatches(t *testing.T) {
	dir := t.TempDir()
	path := place(t, dir, "arm-reference.zim", "small-a.zim")
	m := emptyManifest()
	book := Book{BookID: uuidA, Path: path, ArticleCount: 3, SizeKiB: 1, Element: "kept"}
	current := ArchiveCurrent{File: "arm-reference.zim", BookID: uuidA, ArticleCount: 3, SizeKiB: 1, SHA256: "abc", SourceVersion: "2026-11", SourceFingerprint: "catalog:arm/maxi"}
	m.Books["arm-reference.zim"] = book
	m.Archives["arm-reference"] = current
	r := newFakeReader()

	_, changed := scan(t, dir, r, m)

	assert.False(t, changed)
	assert.Equal(t, current, m.Archives["arm-reference"])
	assert.Equal(t, book, m.Books["arm-reference.zim"])
	assert.Empty(t, r.calls, "a book already recorded with the same id must not go back through kiwix-manage")
}

func TestScanUnmanaged(t *testing.T) {
	dir := t.TempDir()
	place(t, dir, "wikipedia_en_100_2025-10.zim", "small-a.zim")
	m := emptyManifest()

	unmanaged, _ := scan(t, dir, newFakeReader(), m)

	assert.Empty(t, unmanaged)
	assert.Contains(t, m.Books, "wikipedia_en_100_2025-10.zim")
	assert.Empty(t, m.Archives)
}

func TestScanOddFiles(t *testing.T) {
	dir := t.TempDir()
	fifty := strings.Repeat("a", 50) + ".zim"
	place(t, dir, "with space.zim", "small-b.zim")
	place(t, dir, "UPPER.ZIM", "small-a.zim")
	place(t, dir, fifty, "tiny.zim")
	truncated := place(t, dir, "truncated.zim", "truncated.zim")
	require.NoError(t, os.Symlink(filepath.Join(fixturesDir, "small-a.zim"), filepath.Join(dir, "link.zim")))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "dir.zim"), 0o750))
	m := emptyManifest()
	r := newFakeReader()
	r.fail[truncated] = errors.New("kiwix-manage: invalid ZIM")

	unmanaged, _ := scan(t, dir, r, m)

	assert.Equal(t, []libapi.UnmanagedFile{{Name: "truncated.zim", Error: "kiwix-manage: invalid ZIM"}}, unmanaged)
	assert.ElementsMatch(t, []string{"with space.zim", fifty}, mapKeys(m.Books))
	assert.Empty(t, m.Archives, "a 50-character name is not canonical")
}

func TestScanUnreadableHeader(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "junk.zim"), []byte("not a zim file at all, just text"), 0o600))
	m := emptyManifest()
	r := newFakeReader()

	unmanaged, _ := scan(t, dir, r, m)

	require.Len(t, unmanaged, 1)
	assert.Equal(t, "junk.zim", unmanaged[0].Name)
	assert.Contains(t, unmanaged[0].Error, "not a ZIM file")
	assert.Empty(t, m.Books)
	assert.Empty(t, r.calls)
}

func TestScanDuplicateUUID(t *testing.T) {
	dir := t.TempDir()
	place(t, dir, "a.zim", "small-a.zim")
	place(t, dir, "b.zim", "small-a.zim")
	m := emptyManifest()

	unmanaged, _ := scan(t, dir, newFakeReader(), m)

	assert.Equal(t, []libapi.UnmanagedFile{{Name: "b.zim", Error: "duplicate book id of a.zim"}}, unmanaged)
	assert.Equal(t, []string{"a.zim"}, mapKeys(m.Books))
	assert.Equal(t, "a.zim", m.Archives["a"].File)
	assert.NotContains(t, m.Archives, "b")
}

func TestScanDuplicateUUIDEarlierNameWinsOverManifest(t *testing.T) {
	dir := t.TempDir()
	place(t, dir, "a.zim", "small-a.zim")
	path := place(t, dir, "b.zim", "small-a.zim")
	m := emptyManifest()
	m.Books["b.zim"] = Book{BookID: uuidA, Path: path}

	unmanaged, _ := scan(t, dir, newFakeReader(), m)

	assert.Equal(t, []libapi.UnmanagedFile{{Name: "b.zim", Error: "duplicate book id of a.zim"}}, unmanaged)
	assert.Equal(t, []string{"a.zim"}, mapKeys(m.Books))
}

func TestScanDropsDeletedFiles(t *testing.T) {
	dir := t.TempDir()
	m := emptyManifest()
	m.Books["gone.zim"] = Book{BookID: uuidA, Path: filepath.Join(dir, "gone.zim")}
	m.Archives["gone"] = ArchiveCurrent{File: "gone.zim", BookID: uuidA}

	_, changed := scan(t, dir, newFakeReader(), m)

	assert.True(t, changed)
	assert.Empty(t, m.Books)
	assert.Empty(t, m.Archives)
}

func TestScanSkipsPendingUnlinks(t *testing.T) {
	dir := t.TempDir()
	place(t, dir, "retired.zim", "small-a.zim")
	place(t, dir, "kept.zim", "small-b.zim")
	m := emptyManifest()
	m.PendingUnlinks = []string{"retired.zim"}

	_, _ = scan(t, dir, newFakeReader(), m)

	assert.Equal(t, []string{"kept.zim"}, mapKeys(m.Books))
	assert.NotContains(t, m.Archives, "retired")
}

func TestScanRejectsReaderIDMismatch(t *testing.T) {
	dir := t.TempDir()
	path := place(t, dir, "a.zim", "small-a.zim")
	m := emptyManifest()
	r := newFakeReader()
	r.override[path] = Book{BookID: uuidB}

	unmanaged, _ := scan(t, dir, r, m)

	require.Len(t, unmanaged, 1)
	assert.Equal(t, "a.zim", unmanaged[0].Name)
	assert.Contains(t, unmanaged[0].Error, uuidB)
	assert.Empty(t, m.Books)
}

func TestScanSecondRunIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	place(t, dir, "arm-reference.zim", "small-a.zim")
	place(t, dir, "wikipedia_en_100_2025-10.zim", "small-b.zim")
	m := emptyManifest()
	r := newFakeReader()
	scan(t, dir, r, m)
	r.calls = nil

	_, changed := scan(t, dir, r, m)

	assert.False(t, changed)
	assert.Empty(t, r.calls)
}

func TestScanCancelledLeavesManifestUntouched(t *testing.T) {
	dir := t.TempDir()
	place(t, dir, "a.zim", "small-a.zim")
	m := emptyManifest()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, _, err := Scan(ctx, dir, newFakeReader(), m)

	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, m.Books)
}

func TestScanMissingDataDir(t *testing.T) {
	_, _, err := Scan(t.Context(), filepath.Join(t.TempDir(), "absent"), newFakeReader(), emptyManifest())

	require.Error(t, err)
}

func mapKeys[V any](m map[string]V) []string { return slices.Sorted(maps.Keys(m)) }

func TestScanCanonicalWinsDuplicateOverEarlierCopy(t *testing.T) {
	dir := t.TempDir()
	// "arm-reference (copy).zim" sorts before "arm-reference.zim".
	place(t, dir, "arm-reference (copy).zim", "small-a.zim")
	path := place(t, dir, "arm-reference.zim", "small-a.zim")
	m := emptyManifest()
	book := Book{BookID: uuidA, Path: path, ArticleCount: 3, SizeKiB: 1, Element: "kept"}
	current := ArchiveCurrent{File: "arm-reference.zim", BookID: uuidA, ArticleCount: 3, SizeKiB: 1, SHA256: "abc", SourceVersion: "2026-11", SourceFingerprint: "catalog:arm/maxi"}
	m.Books["arm-reference.zim"] = book
	m.Archives["arm-reference"] = current

	unmanaged, _ := scan(t, dir, newFakeReader(), m)

	assert.Equal(t, []libapi.UnmanagedFile{{Name: "arm-reference (copy).zim", Error: "duplicate book id of arm-reference.zim"}}, unmanaged)
	assert.Equal(t, current, m.Archives["arm-reference"])
	assert.Equal(t, book, m.Books["arm-reference.zim"])
	assert.Equal(t, []string{"arm-reference.zim"}, mapKeys(m.Books))
}

func TestScanDuplicateCanonicalSortedOrderWins(t *testing.T) {
	dir := t.TempDir()
	place(t, dir, "a.zim", "small-a.zim")
	place(t, dir, "b.zim", "small-a.zim")
	m := emptyManifest()

	unmanaged, _ := scan(t, dir, newFakeReader(), m)

	assert.Equal(t, []libapi.UnmanagedFile{{Name: "b.zim", Error: "duplicate book id of a.zim"}}, unmanaged)
	assert.Equal(t, []string{"a"}, mapKeys(m.Archives))
}
