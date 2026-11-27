package librarian

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jacaudi/kiwix-operator/internal/libapi"
	"github.com/jacaudi/kiwix-operator/internal/zim"
)

const (
	armRef  = "arm-reference"
	armFile = "arm-reference.zim"
	attA    = "aaaaaaaa"
	attB    = "bbbbbbbb"
)

// testStore is a Store over a temp data dir with a fake reader and a confirmer the test switches on and off.
type testStore struct {
	*Store
	confirm  bool
	changes  []Change
	terminal []string
}

func newTestStore(t *testing.T) *testStore {
	t.Helper()
	return reopenStore(t, t.TempDir(), newManifest())
}

// reopenStore builds a Store over an existing data dir, as a librarian restart would.
func reopenStore(t *testing.T, data string, m *Manifest) *testStore {
	t.Helper()
	ts := &testStore{confirm: true}
	w, _ := newTestWriter(t, func(_ int, c Change) (bool, error) {
		ts.changes = append(ts.changes, c)
		return ts.confirm, nil
	})
	ts.Store = NewStore(data, m, w, newFakeReader())
	ts.OnTerminal = func(attempt string) { ts.terminal = append(ts.terminal, attempt) }
	return ts
}

func (ts *testStore) dataPath(name string) string { return filepath.Join(ts.DataDir, name) }

func (ts *testStore) stagedFile(attempt, ext string) string {
	return filepath.Join(ts.DataDir, ".librarian", "staged", attempt+ext)
}

// stage puts fixture in the staged dir as attempt's ZIM and records the attempt as staged, as Task 11's upload path leaves it.
func (ts *testStore) stage(t *testing.T, attempt, archive, fixture string, parts ...string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(ts.stagedFile(attempt, ".zim")), 0o750))
	path := place(t, filepath.Dir(ts.stagedFile(attempt, ".zim")), attempt+".zim", fixture)
	id, err := zim.ReadUUID(path)
	require.NoError(t, err)
	ts.M.Attempts[attempt] = AttemptRecord{
		// RegisterRequest is spelled out because AttemptRecord.Parts shadows the promoted Parts.
		RegisterRequest: libapi.RegisterRequest{Archive: archive, Attempt: attempt, Kind: libapi.KindUpload, Parts: parts},
		State:           libapi.StateStaged,
		Parts:           map[string]PartRecord{"zim": {SHA256: "sha-" + attempt, Done: true}},
		Staged:          &Book{BookID: id, Path: path, ArticleCount: 3, SizeKiB: 1, Element: `<book id="` + id + `" path="` + path + `" title="T"/>`},
	}
	return id
}

func (ts *testStore) promote(t *testing.T, attempt, expected string) libapi.BookInfo {
	t.Helper()
	info, err := ts.Promote(t.Context(), attempt, libapi.PromoteRequest{ExpectedCurrentBookID: expected, SourceVersion: "2026-11", SourceFingerprint: "url:u"})
	require.NoError(t, err)
	return info
}

func (ts *testStore) state(attempt string) string { return ts.M.Attempts[attempt].State }

// pendingIntent records the intent Promote saves before its rename, as if the librarian crashed right after.
func (ts *testStore) pendingIntent(t *testing.T, attempt, expected string) Intent {
	t.Helper()
	rec := ts.M.Attempts[attempt]
	target := ts.dataPath(rec.Archive + ".zim")
	book := *rec.Staged
	book.Path, book.Element = target, RewritePath(book.Element, target)
	in := Intent{
		Op: opPromote, Archive: rec.Archive, Attempt: attempt, Source: rec.Staged.Path,
		ExpectedCurrentBookID: expected, NewBookID: book.BookID, Book: book,
		Current: ArchiveCurrent{File: rec.Archive + ".zim", BookID: book.BookID, ArticleCount: 3, SizeKiB: 1, SourceVersion: "2026-11"},
	}
	ts.M.Intents = append(ts.M.Intents, in)
	rec.State = libapi.StatePromoting
	ts.M.Attempts[attempt] = rec
	require.NoError(t, ts.M.Save(filepath.Join(ts.DataDir, ".librarian")))
	return in
}

// denyWrites makes dir read-only so the next manifest save fails. Root writes through the mode, as in the kiwix test container, so the test is skipped there.
func denyWrites(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so the save cannot be made to fail")
	}
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })
}

func (ts *testStore) lastChange(t *testing.T) Change {
	t.Helper()
	require.NotEmpty(t, ts.changes)
	return ts.changes[len(ts.changes)-1]
}

func TestPromoteFirstVersion(t *testing.T) {
	ts := newTestStore(t)
	ts.stage(t, attA, armRef, "small-a.zim")

	info := ts.promote(t, attA, "")

	want := libapi.BookInfo{Archive: armRef, File: armFile, BookID: uuidA, ArticleCount: 3, SizeKiB: 1, SHA256: "sha-" + attA, SourceVersion: "2026-11", SourceFingerprint: "url:u"}
	assert.Equal(t, want, info)
	assert.Equal(t, &want, ts.Current(armRef))
	id, err := zim.ReadUUID(ts.dataPath(armFile))
	require.NoError(t, err)
	assert.Equal(t, uuidA, id)
	assert.NoFileExists(t, ts.stagedFile(attA, ".zim"))
	assert.Equal(t, libapi.StatePromoted, ts.state(attA))
	assert.Equal(t, []string{attA}, ts.terminal)
	assert.Equal(t, Change{Present: map[string]string{uuidA: armRef}}, ts.lastChange(t))

	saved, _, err := LoadManifest(filepath.Join(ts.DataDir, ".librarian"))
	require.NoError(t, err)
	assert.Empty(t, saved.Intents)
	assert.Equal(t, uuidA, saved.Archives[armRef].BookID)
	assert.Equal(t, libapi.StatePromoted, saved.Attempts[attA].State)
}

func TestPromoteReplacesCurrent(t *testing.T) {
	ts := newTestStore(t)
	ts.stage(t, attA, armRef, "small-a.zim")
	ts.promote(t, attA, "")
	ts.stage(t, attB, armRef, "small-b.zim")

	info := ts.promote(t, attB, uuidA)

	assert.Equal(t, uuidB, info.BookID)
	assert.Equal(t, Change{Present: map[string]string{uuidB: armRef}, Absent: []string{uuidA}}, ts.lastChange(t))
	assert.Equal(t, []string{armFile}, mapKeys(ts.M.Books))
}

func TestPromoteIdempotent(t *testing.T) {
	ts := newTestStore(t)
	ts.stage(t, attA, armRef, "small-a.zim")
	first := ts.promote(t, attA, "")
	writes := len(ts.changes)

	second := ts.promote(t, attA, "")

	assert.Equal(t, first, second)
	assert.Len(t, ts.changes, writes, "a repeated promote must not write library.xml again")
	assert.Equal(t, []string{attA}, ts.terminal)
}

func TestPromoteCASMismatch(t *testing.T) {
	ts := newTestStore(t)
	ts.stage(t, attA, armRef, "small-a.zim")

	_, err := ts.Promote(t.Context(), attA, libapi.PromoteRequest{ExpectedCurrentBookID: "zzz"})

	require.ErrorIs(t, err, ErrCurrentChanged)
	assert.FileExists(t, ts.stagedFile(attA, ".zim"))
	assert.NoFileExists(t, ts.dataPath(armFile))
	assert.Equal(t, libapi.StateStaged, ts.state(attA))
	assert.Empty(t, ts.M.Intents)
	assert.Empty(t, ts.changes)
	assert.NoFileExists(t, filepath.Join(ts.DataDir, ".librarian", "manifest.json"), "a refused promote writes nothing")
}

func TestPromoteRejectsUnknownOrUnstaged(t *testing.T) {
	ts := newTestStore(t)
	ts.M.Attempts[attB] = AttemptRecord{Archive: armRef, Attempt: attB, State: libapi.StateRegistered}

	_, err := ts.Promote(t.Context(), attA, libapi.PromoteRequest{})
	require.ErrorIs(t, err, ErrNotFound)

	_, err = ts.Promote(t.Context(), attB, libapi.PromoteRequest{})
	require.ErrorIs(t, err, ErrConflict)
	assert.Equal(t, libapi.StateRegistered, ts.state(attB))
}

func TestPromoteDuplicateUnmanaged(t *testing.T) {
	ts := newTestStore(t)
	place(t, ts.DataDir, "devdocs_en_python_2026-10.zim", "small-a.zim")
	require.NoError(t, ts.Rescan(t.Context()))
	ts.stage(t, attA, armRef, "small-a.zim")

	_, err := ts.Promote(t.Context(), attA, libapi.PromoteRequest{})

	require.ErrorIs(t, err, ErrDuplicateBook)
	assert.Contains(t, err.Error(), "devdocs_en_python_2026-10.zim")
	assert.Contains(t, err.Error(), "adoptFile")
	assert.NoFileExists(t, ts.dataPath(armFile))
	assert.Equal(t, libapi.StateStaged, ts.state(attA))
}

func TestPromoteMovesWarcs(t *testing.T) {
	ts := newTestStore(t)
	tar := filepath.Join(ts.DataDir, ".librarian", "warcs", armRef+".tar")
	require.NoError(t, os.MkdirAll(filepath.Dir(tar), 0o750))
	require.NoError(t, os.WriteFile(tar, []byte("old"), 0o600))
	ts.stage(t, attA, armRef, "small-a.zim", "zim", "warcs")
	require.NoError(t, os.WriteFile(ts.stagedFile(attA, ".warcs"), []byte("new"), 0o600))

	ts.promote(t, attA, "")

	got, err := os.ReadFile(tar)
	require.NoError(t, err)
	assert.Equal(t, "new", string(got))
	assert.NoFileExists(t, ts.stagedFile(attA, ".warcs"))
}

func TestPromoteRewritesPath(t *testing.T) {
	ts := newTestStore(t)
	ts.stage(t, attA, armRef, "small-a.zim")

	ts.promote(t, attA, "")

	lib, err := os.ReadFile(ts.W.Path)
	require.NoError(t, err)
	assert.Contains(t, string(lib), `path="`+ts.dataPath(armFile)+`"`)
	assert.NotContains(t, string(lib), "staged")
}

func TestPromoteRejectsElementWithoutPath(t *testing.T) {
	ts := newTestStore(t)
	ts.stage(t, attA, armRef, "small-a.zim")
	rec := ts.M.Attempts[attA]
	rec.Staged.Element = `<book id="` + uuidA + `"/>`

	_, err := ts.Promote(t.Context(), attA, libapi.PromoteRequest{})

	require.Error(t, err)
	assert.FileExists(t, ts.stagedFile(attA, ".zim"))
	assert.Empty(t, ts.M.Intents)
	assert.Empty(t, ts.changes)
}

func TestPromoteFinishesAfterClientCancels(t *testing.T) {
	ts := newTestStore(t)
	ts.stage(t, attA, armRef, "small-a.zim")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	info, err := ts.Promote(ctx, attA, libapi.PromoteRequest{})

	require.NoError(t, err)
	assert.Equal(t, uuidA, info.BookID)
	assert.Empty(t, ts.M.Intents, "once the intent is saved the protocol runs to its commit")
	assert.Equal(t, libapi.StatePromoted, ts.state(attA))
}

func TestFailedSaveLeavesMemoryAsOnDisk(t *testing.T) {
	ts := newTestStore(t)
	ts.stage(t, attA, armRef, "small-a.zim")
	dir := filepath.Join(ts.DataDir, ".librarian")
	require.NoError(t, ts.M.Save(dir))
	denyWrites(t, dir)

	_, err := ts.Promote(t.Context(), attA, libapi.PromoteRequest{})

	require.Error(t, err)
	assert.Empty(t, ts.M.Intents)
	assert.Equal(t, libapi.StateStaged, ts.state(attA))
	assert.FileExists(t, ts.stagedFile(attA, ".zim"))
}

func TestOnTerminalRunsAfterUnlock(t *testing.T) {
	ts := newTestStore(t)
	ts.stage(t, attA, armRef, "small-a.zim")
	var locked bool
	ts.OnTerminal = func(string) {
		if locked = !ts.mu.TryLock(); !locked {
			ts.mu.Unlock()
		}
	}

	ts.promote(t, attA, "")

	assert.False(t, locked, "OnTerminal must run after s.mu is released")
}

func TestSnapshotUnchangedByLaterMutation(t *testing.T) {
	ts := newTestStore(t)
	ts.stage(t, attA, armRef, "small-a.zim")
	require.NoError(t, ts.Rescan(t.Context()))
	before := ts.Snapshot()

	ts.promote(t, attA, "")

	assert.Equal(t, libapi.StateStaged, before.Attempts[attA].State)
	assert.Nil(t, before.Attempts[attA].Current)
	assert.NotContains(t, before.Archives, armRef)
	assert.Empty(t, before.Library.Books)
	after := ts.Snapshot()
	assert.Equal(t, libapi.StatePromoted, after.Attempts[attA].State)
	assert.Equal(t, uuidA, after.Attempts[attA].Current.BookID)
	assert.Equal(t, []libapi.BookInfo{after.Archives[armRef]}, after.Library.Books)
}

func TestRetainedFileIsCurrentForRecreatedArchive(t *testing.T) {
	for _, keepManifest := range []bool{true, false} {
		t.Run(map[bool]string{true: "manifest kept", false: "manifest lost"}[keepManifest], func(t *testing.T) {
			ts := newTestStore(t)
			ts.stage(t, attA, armRef, "small-a.zim")
			ts.promote(t, attA, "")

			done, err := ts.Delete(t.Context(), armRef, "Retain")
			require.NoError(t, err)
			require.True(t, done)

			m := newManifest()
			if keepManifest {
				m, _, err = LoadManifest(filepath.Join(ts.DataDir, ".librarian"))
				require.NoError(t, err)
			}
			fresh := reopenStore(t, ts.DataDir, m)
			require.NoError(t, fresh.Rescan(t.Context()))
			cur := fresh.Current(armRef)
			require.NotNil(t, cur)
			assert.Equal(t, uuidA, cur.BookID)
		})
	}
}

func TestRescanWritesScanChanges(t *testing.T) {
	ts := newTestStore(t)
	place(t, ts.DataDir, "a.zim", "small-a.zim")
	require.NoError(t, ts.Rescan(t.Context()))
	assert.Equal(t, Change{Present: map[string]string{uuidA: "a"}}, ts.lastChange(t))

	require.NoError(t, os.Remove(ts.dataPath("a.zim")))
	place(t, ts.DataDir, "B Side.zim", "small-b.zim")
	require.NoError(t, ts.Rescan(t.Context()))

	assert.Equal(t, Change{Present: map[string]string{uuidB: "b_side"}, Absent: []string{uuidA}}, ts.lastChange(t))
	assert.Equal(t, []libapi.BookInfo{{File: "B Side.zim", BookID: uuidB, ArticleCount: 3, SizeKiB: 1}}, ts.Snapshot().Library.Books)
}

func TestRollForwardRechecksCAS(t *testing.T) {
	ts := newTestStore(t)
	ts.stage(t, attA, armRef, "small-a.zim")
	ts.pendingIntent(t, attA, "")
	ts.M.Archives[armRef] = ArchiveCurrent{File: armFile, BookID: uuidB}

	require.NoError(t, ts.RollForward(t.Context()))

	assert.Empty(t, ts.M.Intents)
	assert.FileExists(t, ts.stagedFile(attA, ".zim"))
	assert.NoFileExists(t, ts.dataPath(armFile))
	assert.Equal(t, libapi.StateStaged, ts.state(attA))
	assert.Equal(t, uuidB, ts.M.Archives[armRef].BookID)
	assert.Empty(t, ts.changes)
}

func TestRollForwardBeforeRename(t *testing.T) {
	ts := newTestStore(t)
	ts.stage(t, attA, armRef, "small-a.zim")
	ts.pendingIntent(t, attA, "")

	require.NoError(t, ts.RollForward(t.Context()))

	assert.NoFileExists(t, ts.stagedFile(attA, ".zim"))
	assert.Equal(t, uuidA, ts.Current(armRef).BookID)
	assert.Equal(t, "2026-11", ts.Current(armRef).SourceVersion)
	assert.Equal(t, libapi.StatePromoted, ts.state(attA))
	assert.Equal(t, []string{attA}, ts.terminal)
	assert.Empty(t, ts.M.Intents)
}

func TestRollForwardAfterRename(t *testing.T) {
	ts := newTestStore(t)
	ts.stage(t, attA, armRef, "small-a.zim")
	ts.pendingIntent(t, attA, "")
	require.NoError(t, os.Rename(ts.stagedFile(attA, ".zim"), ts.dataPath(armFile)))

	require.NoError(t, ts.RollForward(t.Context()))

	assert.Equal(t, Change{Present: map[string]string{uuidA: armRef}}, ts.lastChange(t))
	assert.Equal(t, uuidA, ts.Current(armRef).BookID)
	assert.Equal(t, libapi.StatePromoted, ts.state(attA))
	assert.Empty(t, ts.M.Intents)
	saved, _, err := LoadManifest(filepath.Join(ts.DataDir, ".librarian"))
	require.NoError(t, err)
	assert.Empty(t, saved.Intents)
	assert.Equal(t, uuidA, saved.Archives[armRef].BookID)
}

func TestRollForwardMovesWarcs(t *testing.T) {
	tests := []struct {
		name        string
		renameFirst bool
	}{
		{name: "before the rename"},
		{name: "after the rename", renameFirst: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestStore(t)
			tar := filepath.Join(ts.DataDir, ".librarian", "warcs", armRef+".tar")
			ts.stage(t, attA, armRef, "small-a.zim", "zim", "warcs")
			require.NoError(t, os.WriteFile(ts.stagedFile(attA, ".warcs"), []byte("new"), 0o600))
			ts.pendingIntent(t, attA, "")
			if tt.renameFirst {
				require.NoError(t, os.Rename(ts.stagedFile(attA, ".zim"), ts.dataPath(armFile)))
			}

			require.NoError(t, ts.RollForward(t.Context()))

			got, err := os.ReadFile(tar)
			require.NoError(t, err)
			assert.Equal(t, "new", string(got))
			assert.NoFileExists(t, ts.stagedFile(attA, ".warcs"))
			assert.Equal(t, libapi.StatePromoted, ts.state(attA))
		})
	}
}

func TestRollForwardKeepsWarcsAlreadyMoved(t *testing.T) {
	ts := newTestStore(t)
	tar := filepath.Join(ts.DataDir, ".librarian", "warcs", armRef+".tar")
	require.NoError(t, os.MkdirAll(filepath.Dir(tar), 0o750))
	require.NoError(t, os.WriteFile(tar, []byte("moved"), 0o600))
	ts.stage(t, attA, armRef, "small-a.zim", "zim", "warcs")
	ts.pendingIntent(t, attA, "")

	require.NoError(t, ts.RollForward(t.Context()))

	got, err := os.ReadFile(tar)
	require.NoError(t, err)
	assert.Equal(t, "moved", string(got), "a tar the crashed promotion already moved is left alone")
	assert.Equal(t, libapi.StatePromoted, ts.state(attA))
}

func TestPromoteRefusesDirectoryTarget(t *testing.T) {
	ts := newTestStore(t)
	ts.stage(t, attA, armRef, "small-a.zim")
	require.NoError(t, os.Mkdir(ts.dataPath(armFile), 0o750))

	_, err := ts.Promote(t.Context(), attA, libapi.PromoteRequest{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), armFile)
	assert.Empty(t, ts.M.Intents)
	assert.Equal(t, libapi.StateStaged, ts.state(attA))
	assert.FileExists(t, ts.stagedFile(attA, ".zim"))
	assert.Empty(t, ts.changes)
}

func TestRollForwardContinuesPastFailingIntent(t *testing.T) {
	ts := newTestStore(t)
	ts.stage(t, attA, armRef, "small-a.zim")
	ts.stage(t, attB, "other", "small-b.zim")
	ts.pendingIntent(t, attA, "")
	ts.pendingIntent(t, attB, "")
	// A directory in the way makes the first intent's rename fail on every restart.
	require.NoError(t, os.Mkdir(ts.dataPath(armFile), 0o750))

	err := ts.RollForward(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), armRef)
	require.NotNil(t, ts.Current("other"), "the second intent must still roll forward")
	assert.Equal(t, uuidB, ts.Current("other").BookID)
	assert.Equal(t, libapi.StatePromoted, ts.state(attB))
	assert.Equal(t, []string{attB}, ts.terminal)
}

func TestRollForwardSourceGoneUUIDMismatch(t *testing.T) {
	ts := newTestStore(t)
	ts.stage(t, attA, armRef, "small-a.zim")
	ts.pendingIntent(t, attA, "")
	require.NoError(t, os.Remove(ts.stagedFile(attA, ".zim")))
	place(t, ts.DataDir, armFile, "small-b.zim")

	require.NoError(t, ts.RollForward(t.Context()))

	assert.Empty(t, ts.M.Intents)
	assert.Nil(t, ts.Current(armRef))
	assert.Equal(t, libapi.StateFailed, ts.state(attA))
	assert.Equal(t, []string{attA}, ts.terminal)
	assert.Empty(t, ts.changes)
}

func TestAdoptRenamesAndKeepsID(t *testing.T) {
	ts := newTestStore(t)
	place(t, ts.DataDir, "bg3.wiki_0e9fc406.zim", "small-a.zim")
	require.NoError(t, ts.Rescan(t.Context()))

	info, err := ts.Adopt(t.Context(), "bg3-wiki", libapi.AdoptRequest{File: "bg3.wiki_0e9fc406.zim"})

	require.NoError(t, err)
	assert.Equal(t, libapi.BookInfo{Archive: "bg3-wiki", File: "bg3-wiki.zim", BookID: uuidA, ArticleCount: 3, SizeKiB: 1, SourceVersion: "2026-10"}, info)
	assert.NoFileExists(t, ts.dataPath("bg3.wiki_0e9fc406.zim"))
	assert.FileExists(t, ts.dataPath("bg3-wiki.zim"))
	assert.Equal(t, Change{Present: map[string]string{uuidA: "bg3-wiki"}}, ts.lastChange(t))
	assert.Equal(t, []string{"bg3-wiki.zim"}, mapKeys(ts.M.Books))
	assert.Equal(t, uuidA, ts.Current("bg3-wiki").BookID)
	assert.Empty(t, ts.M.Intents)
}

func TestAdoptSourceVersion(t *testing.T) {
	tests := []struct{ file, want string }{
		{"devdocs_en_python_2026-03.zim", "2026-03"},
		{"bg3.wiki_0e9fc406.zim", "2026-10"},
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			ts := newTestStore(t)
			place(t, ts.DataDir, tc.file, "small-a.zim")

			info, err := ts.Adopt(t.Context(), "docs", libapi.AdoptRequest{File: tc.file})

			require.NoError(t, err)
			assert.Equal(t, tc.want, info.SourceVersion)
		})
	}
}

func TestAdoptRejectsSymlinkAndManaged(t *testing.T) {
	ts := newTestStore(t)
	place(t, ts.DataDir, "other.zim", "small-a.zim")
	require.NoError(t, os.Symlink(filepath.Join(ts.DataDir, "other.zim"), ts.dataPath("link_2026-01.zim")))
	require.NoError(t, os.Mkdir(ts.dataPath("dir_2026-01.zim"), 0o750))

	tests := []struct {
		file string
		want error
	}{
		{"link_2026-01.zim", ErrAdoptInvalid},
		{"other.zim", ErrAdoptInvalid},
		{"dir_2026-01.zim", ErrAdoptInvalid},
		{"../other_2026-01.zim", ErrAdoptInvalid},
		{"missing_2026-01.zim", ErrAdoptMissing},
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			_, err := ts.Adopt(t.Context(), "bg3-wiki", libapi.AdoptRequest{File: tc.file})

			require.ErrorIs(t, err, tc.want)
			assert.NoFileExists(t, ts.dataPath("bg3-wiki.zim"))
		})
	}
}

func TestDeleteUnlinksOnlyAfterConfirm(t *testing.T) {
	ts := newTestStore(t)
	ts.stage(t, attA, armRef, "small-a.zim")
	ts.promote(t, attA, "")
	ts.confirm = false

	done, err := ts.Delete(t.Context(), armRef, "Delete")

	require.NoError(t, err)
	assert.False(t, done)
	assert.FileExists(t, ts.dataPath(armFile))
	assert.Equal(t, []string{armFile}, ts.M.PendingUnlinks)
	assert.Equal(t, []string{uuidA}, ts.lastChange(t).Absent)
	assert.Nil(t, ts.Current(armRef))
	assert.True(t, ts.Snapshot().Library.ReloadUnconfirmed)

	ts.confirm = true
	require.NoError(t, ts.Rescan(t.Context()))

	assert.NoFileExists(t, ts.dataPath(armFile))
	assert.Empty(t, ts.M.PendingUnlinks)
	assert.Contains(t, ts.lastChange(t).Absent, uuidA, "the retry must prove the book is gone before unlinking")
	assert.False(t, ts.Snapshot().Library.ReloadUnconfirmed)
}

func TestDeleteKeepsWarcsOnRetain(t *testing.T) {
	tests := []struct {
		policy string
		kept   bool
	}{
		{"Retain", true},
		{"Delete", false},
	}
	for _, tc := range tests {
		t.Run(tc.policy, func(t *testing.T) {
			ts := newTestStore(t)
			ts.stage(t, attA, armRef, "small-a.zim", "zim", "warcs")
			require.NoError(t, os.WriteFile(ts.stagedFile(attA, ".warcs"), []byte("w"), 0o600))
			ts.promote(t, attA, "")
			tar := filepath.Join(ts.DataDir, ".librarian", "warcs", armRef+".tar")

			done, err := ts.Delete(t.Context(), armRef, tc.policy)

			require.NoError(t, err)
			assert.True(t, done)
			if tc.kept {
				assert.FileExists(t, tar)
				assert.FileExists(t, ts.dataPath(armFile))
			} else {
				assert.NoFileExists(t, tar)
				assert.NoFileExists(t, ts.dataPath(armFile))
			}
		})
	}
}

func TestDeleteRejectsUnknownPolicy(t *testing.T) {
	ts := newTestStore(t)

	_, err := ts.Delete(t.Context(), armRef, "Orphan")

	require.Error(t, err)
}

func TestDeleteDiscardsOpenAttempts(t *testing.T) {
	ts := newTestStore(t)
	ts.stage(t, attA, armRef, "small-a.zim")
	ts.promote(t, attA, "")
	ts.terminal = nil
	ts.M.Attempts[attB] = AttemptRecord{Archive: armRef, Attempt: attB, State: libapi.StateRegistered}
	place(t, filepath.Dir(ts.stagedFile(attB, "")), attB+".zim.part", "small-b.zim")

	done, err := ts.Delete(t.Context(), armRef, "Retain")

	require.NoError(t, err)
	assert.True(t, done)
	assert.Equal(t, []string{attB}, ts.terminal, "only the open attempt becomes terminal")
	assert.NoFileExists(t, ts.stagedFile(attB, ".zim.part"))
	assert.Empty(t, ts.M.Attempts)
	assert.Empty(t, ts.Snapshot().Attempts)
}

func TestDeleteDropsPendingIntent(t *testing.T) {
	ts := newTestStore(t)
	ts.stage(t, attA, armRef, "small-a.zim")
	ts.pendingIntent(t, attA, "")
	require.NoError(t, os.Rename(ts.stagedFile(attA, ".zim"), ts.dataPath(armFile)))
	// Unconfirmed, so the file is still on disk when roll-forward looks at it.
	ts.confirm = false

	_, err := ts.Delete(t.Context(), armRef, "Delete")
	require.NoError(t, err)
	require.NoError(t, ts.RollForward(t.Context()))

	assert.Empty(t, ts.M.Intents)
	assert.Nil(t, ts.Current(armRef), "a deleted archive must not be resurrected by roll-forward")
	assert.Equal(t, []string{armFile}, ts.M.PendingUnlinks)
}

func TestUnlinkSkipsServedFiles(t *testing.T) {
	ts := newTestStore(t)
	place(t, ts.DataDir, "served.zim", "small-a.zim")
	require.NoError(t, ts.Rescan(t.Context()))
	ts.M.PendingUnlinks = []string{"served.zim", "gone.zim"}

	ts.mu.Lock()
	confirmed, err := ts.write(t.Context(), Change{})
	ts.mu.Unlock()

	require.NoError(t, err)
	assert.True(t, confirmed)
	assert.FileExists(t, ts.dataPath("served.zim"))
	assert.Empty(t, ts.M.PendingUnlinks, "a served file leaves the list without being unlinked, a missing one is simply dropped")
}

func TestRepromoteClearsPendingUnlink(t *testing.T) {
	ts := newTestStore(t)
	ts.stage(t, attA, armRef, "small-a.zim")
	ts.promote(t, attA, "")
	ts.confirm = false
	done, err := ts.Delete(t.Context(), armRef, "Delete")
	require.NoError(t, err)
	require.False(t, done)
	ts.stage(t, attB, armRef, "small-b.zim")

	// Still unconfirmed, so only the intent's save can have cleared the pending unlink.
	ts.promote(t, attB, "")

	assert.Empty(t, ts.M.PendingUnlinks)
	m, _, err := LoadManifest(filepath.Join(ts.DataDir, ".librarian"))
	require.NoError(t, err)
	fresh := reopenStore(t, ts.DataDir, m)
	require.NoError(t, fresh.Rescan(t.Context()))
	assert.FileExists(t, ts.dataPath(armFile))
	assert.Equal(t, uuidB, fresh.Current(armRef).BookID)
}

func TestAdoptRecordsSourceFingerprint(t *testing.T) {
	ts := newTestStore(t)
	place(t, ts.DataDir, "devdocs_en_go_2026-10.zim", "small-a.zim")

	info, err := ts.Adopt(t.Context(), "docs", libapi.AdoptRequest{File: "devdocs_en_go_2026-10.zim", SourceFingerprint: "catalog:devdocs_en_go/"})

	require.NoError(t, err)
	assert.Equal(t, "catalog:devdocs_en_go/", info.SourceFingerprint)
	assert.Equal(t, "catalog:devdocs_en_go/", ts.Current("docs").SourceFingerprint)
}

func TestUnlinkIsDurableBeforeManifestForgetsIt(t *testing.T) {
	ts := newTestStore(t)
	ts.stage(t, attA, armRef, "small-a.zim")
	ts.promote(t, attA, "")
	var synced []string
	ts.Sync = func(dir string) error {
		synced = append(synced, dir)
		assert.NoFileExists(t, ts.dataPath(armFile), "the directory is synced after the unlink")
		m, _, err := LoadManifest(filepath.Join(ts.DataDir, ".librarian"))
		require.NoError(t, err)
		assert.Equal(t, []string{armFile}, m.PendingUnlinks, "the saved manifest keeps the file until the unlink is durable")
		return nil
	}

	done, err := ts.Delete(t.Context(), armRef, "Delete")

	require.NoError(t, err)
	assert.True(t, done)
	assert.Equal(t, []string{ts.DataDir}, synced)
	assert.Empty(t, ts.M.PendingUnlinks)
}

func TestFailedUnlinkSyncKeepsPendingUnlink(t *testing.T) {
	ts := newTestStore(t)
	ts.stage(t, attA, armRef, "small-a.zim")
	ts.promote(t, attA, "")
	ts.Sync = func(string) error { return errors.New("fsync failed") }

	done, err := ts.Delete(t.Context(), armRef, "Delete")

	require.Error(t, err)
	assert.False(t, done)
	assert.Equal(t, []string{armFile}, ts.M.PendingUnlinks)
	m, _, loadErr := LoadManifest(filepath.Join(ts.DataDir, ".librarian"))
	require.NoError(t, loadErr)
	assert.Equal(t, []string{armFile}, m.PendingUnlinks, "a retry must still know to unlink it")
}
