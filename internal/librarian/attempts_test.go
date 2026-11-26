package librarian

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jacaudi/kiwix-operator/internal/libapi"
)

const (
	testToken = "upload-token"
	attC      = "cccccccc"
	attD      = "dddddddd"
	day       = 24 * time.Hour
)

var clockA = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

// newTestAttempts is an Attempts over a test store with plenty of free space and a fixed clock.
func newTestAttempts(t *testing.T) (*Attempts, *testStore) {
	t.Helper()
	ts := newTestStore(t)
	ts.Free = func(string) (int64, error) { return 1 << 40, nil }
	ts.Now = func() time.Time { return clockA }
	return &Attempts{S: ts.Store, Now: func() time.Time { return clockA }, IdleTimeout: time.Second}, ts
}

func hexSHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func uploadReq(attempt string, parts ...string) libapi.RegisterRequest {
	return libapi.RegisterRequest{Archive: armRef, Attempt: attempt, Kind: libapi.KindUpload, TokenHash: hexSHA([]byte(testToken)), Parts: parts}
}

func fetchReq(attempt string) libapi.RegisterRequest {
	return libapi.RegisterRequest{Archive: armRef, Attempt: attempt, Kind: libapi.KindFetch, TokenHash: hexSHA([]byte(testToken)), Fetch: &libapi.FetchSpec{URL: "https://example.org/a.zim"}}
}

func register(t *testing.T, a *Attempts, req libapi.RegisterRequest) libapi.AttemptState {
	t.Helper()
	st, err := a.Register(req)
	require.NoError(t, err)
	return st
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixturesDir, name))
	require.NoError(t, err)
	return data
}

func put(t *testing.T, a *Attempts, attempt, part string, data []byte) (libapi.AttemptState, error) {
	t.Helper()
	return a.PutPart(t.Context(), attempt, part, testToken, hexSHA(data), int64(len(data)), bytes.NewReader(data))
}

// setReceiving records attempt as receiving into a partial file that exists, as a crash mid-body leaves it.
func (ts *testStore) setReceiving(t *testing.T, attempt string) string {
	t.Helper()
	partial := ts.stagedFile(attempt, ".zim.part")
	require.NoError(t, os.MkdirAll(filepath.Dir(partial), 0o750))
	require.NoError(t, os.WriteFile(partial, []byte("half"), 0o600))
	rec := ts.M.Attempts[attempt]
	rec.State, rec.PartialPath = libapi.StateReceiving, partial
	ts.M.Attempts[attempt] = rec
	return partial
}

func TestRegisterIdempotent(t *testing.T) {
	a, _ := newTestAttempts(t)
	req := uploadReq(attA, "zim")

	first := register(t, a, req)
	second := register(t, a, req)

	assert.Equal(t, libapi.AttemptState{Archive: armRef, Attempt: attA, Kind: libapi.KindUpload, State: libapi.StateRegistered}, first)
	assert.Equal(t, first, second)
	req.TokenHash = hexSHA([]byte("other"))
	_, err := a.Register(req)
	require.ErrorIs(t, err, ErrConflict)
	other := uploadReq(attA, "zim")
	other.Archive = "other"
	_, err = a.Register(other)
	require.ErrorIs(t, err, ErrConflict)
}

func TestRegisterFetchCallsHook(t *testing.T) {
	a, _ := newTestAttempts(t)
	var calls []string
	a.OnFetchRegistered = func(attempt string) { calls = append(calls, attempt) }

	register(t, a, fetchReq(attA))
	register(t, a, fetchReq(attA))
	register(t, a, uploadReq(attB, "zim"))

	assert.Equal(t, []string{attA}, calls)
}

func TestRegisterRejectsInvalid(t *testing.T) {
	tests := []struct {
		name string
		edit func(*libapi.RegisterRequest)
	}{
		{"attempt with a path", func(r *libapi.RegisterRequest) { r.Attempt = "../aaaaa" }},
		{"attempt outside base32", func(r *libapi.RegisterRequest) { r.Attempt = "aaaaaaa1" }},
		{"archive not a DNS label", func(r *libapi.RegisterRequest) { r.Archive = "Arm_Ref" }},
		{"unknown kind", func(r *libapi.RegisterRequest) { r.Kind = "crawl" }},
		{"upload without zim", func(r *libapi.RegisterRequest) { r.Parts = []string{"warcs"} }},
		{"upload with unknown part", func(r *libapi.RegisterRequest) { r.Parts = []string{"zim", "../x"} }},
		{"fetch without URL", func(r *libapi.RegisterRequest) { *r = fetchReq(attA); r.Fetch = nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, ts := newTestAttempts(t)
			req := uploadReq(attA, "zim")
			tc.edit(&req)

			_, err := a.Register(req)

			require.ErrorIs(t, err, ErrInvalid)
			assert.Empty(t, ts.M.Attempts)
		})
	}
}

func TestPartPutHappyPath(t *testing.T) {
	a, ts := newTestAttempts(t)
	register(t, a, uploadReq(attA, "zim"))
	data := fixture(t, "small-a.zim")

	st, err := put(t, a, attA, "zim", data)

	require.NoError(t, err)
	assert.Equal(t, libapi.StateStaged, st.State)
	require.NotNil(t, st.Staged)
	assert.Equal(t, uuidA, st.Staged.BookID)
	assert.Equal(t, hexSHA(data), st.Staged.SHA256)
	assert.FileExists(t, ts.stagedFile(attA, ".zim"))
	assert.NoFileExists(t, ts.stagedFile(attA, ".zim.part"))
	assert.Empty(t, ts.M.Attempts[attA].PartialPath)
	assert.Equal(t, clockA, ts.M.Attempts[attA].UpdatedAt)
	assert.Equal(t, uuidA, ts.promote(t, attA, "").BookID, "an uploaded attempt promotes")
}

func TestPartPutWrongToken(t *testing.T) {
	a, ts := newTestAttempts(t)
	register(t, a, uploadReq(attA, "zim"))
	data := fixture(t, "small-a.zim")

	_, err := a.PutPart(t.Context(), attA, "zim", "wrong", hexSHA(data), int64(len(data)), bytes.NewReader(data))
	require.ErrorIs(t, err, ErrUnauthorized)
	_, err = put(t, a, attB, "zim", data)
	require.ErrorIs(t, err, ErrNotFound)

	assert.Equal(t, libapi.StateRegistered, ts.state(attA))
	assert.NoFileExists(t, ts.stagedFile(attA, ".zim.part"))
}

func TestPartPutRejectsUnregisteredPart(t *testing.T) {
	a, ts := newTestAttempts(t)
	register(t, a, uploadReq(attA, "zim"))
	register(t, a, fetchReq(attB))
	data := fixture(t, "small-a.zim")

	_, err := put(t, a, attA, "warcs", data)
	require.ErrorIs(t, err, ErrConflict)
	_, err = put(t, a, attB, "zim", data)
	require.ErrorIs(t, err, ErrConflict, "a fetch attempt takes no uploads")

	assert.Equal(t, libapi.StateRegistered, ts.state(attA))
	assert.Equal(t, libapi.StateRegistered, ts.state(attB))
}

func TestPartPutIdempotentOnMatchingHash(t *testing.T) {
	a, ts := newTestAttempts(t)
	register(t, a, uploadReq(attA, "zim", "warcs"))
	data := fixture(t, "small-a.zim")
	first, err := put(t, a, attA, "zim", data)
	require.NoError(t, err)

	again, err := put(t, a, attA, "zim", data)
	require.NoError(t, err)
	assert.Equal(t, first, again)
	assert.Equal(t, libapi.StateRegistered, again.State)

	_, err = put(t, a, attA, "zim", fixture(t, "small-b.zim"))
	require.ErrorIs(t, err, ErrConflict)
	assert.Equal(t, hexSHA(data), ts.M.Attempts[attA].Parts["zim"].SHA256)
}

func TestPartPutHashMismatch(t *testing.T) {
	a, ts := newTestAttempts(t)
	register(t, a, uploadReq(attA, "zim"))
	data := fixture(t, "small-a.zim")

	_, err := a.PutPart(t.Context(), attA, "zim", testToken, hexSHA([]byte("other")), int64(len(data)), bytes.NewReader(data))

	require.ErrorIs(t, err, ErrHashMismatch)
	assert.NoFileExists(t, ts.stagedFile(attA, ".zim.part"))
	assert.NoFileExists(t, ts.stagedFile(attA, ".zim"))
	assert.Equal(t, libapi.StateRegistered, ts.state(attA))
	assert.Empty(t, ts.M.Attempts[attA].PartialPath)
}

func TestStagedOnlyWhenAllParts(t *testing.T) {
	a, ts := newTestAttempts(t)
	register(t, a, uploadReq(attA, "zim", "warcs"))

	st, err := put(t, a, attA, "zim", fixture(t, "small-a.zim"))
	require.NoError(t, err)
	assert.Equal(t, libapi.StateRegistered, st.State)
	assert.Nil(t, st.Staged)

	st, err = put(t, a, attA, "warcs", []byte("warc tar"))
	require.NoError(t, err)
	assert.Equal(t, libapi.StateStaged, st.State)
	assert.FileExists(t, ts.stagedFile(attA, ".zim"))
	assert.FileExists(t, ts.stagedFile(attA, ".warcs"))
}

func TestResetReceivingUploadsOnly(t *testing.T) {
	a, ts := newTestAttempts(t)
	register(t, a, uploadReq(attA, "zim"))
	register(t, a, fetchReq(attB))
	upload := ts.setReceiving(t, attA)
	fetch := ts.setReceiving(t, attB)

	require.NoError(t, a.ResetReceiving())

	assert.Equal(t, libapi.StateRegistered, ts.state(attA))
	assert.Empty(t, ts.M.Attempts[attA].PartialPath)
	assert.NoFileExists(t, upload)
	assert.Equal(t, libapi.StateReceiving, ts.state(attB))
	assert.FileExists(t, fetch, "a fetch resumes from its partial")
	saved, _, err := LoadManifest(filepath.Join(ts.DataDir, ".librarian"))
	require.NoError(t, err)
	assert.Equal(t, libapi.StateRegistered, saved.Attempts[attA].State)
}

func TestIdleTimeout(t *testing.T) {
	t.Run("silent body", func(t *testing.T) {
		a, ts := newTestAttempts(t)
		a.IdleTimeout = 50 * time.Millisecond
		register(t, a, uploadReq(attA, "zim"))
		body, w := io.Pipe()
		// Released only after PutPart returns, so the abandoned read is still blocked when the timeout fires.
		t.Cleanup(func() { _ = w.Close() })

		_, err := a.PutPart(t.Context(), attA, "zim", testToken, hexSHA([]byte("x")), 1, body)

		require.ErrorIs(t, err, os.ErrDeadlineExceeded)
		assert.Equal(t, libapi.StateRegistered, ts.state(attA))
		assert.NoFileExists(t, ts.stagedFile(attA, ".zim.part"))
	})
	t.Run("steady body slower in total than the timeout", func(t *testing.T) {
		a, _ := newTestAttempts(t)
		a.IdleTimeout = 250 * time.Millisecond
		register(t, a, uploadReq(attA, "zim"))
		data := fixture(t, "small-a.zim")
		body, w := io.Pipe()
		go func() {
			for _, chunk := range chunks(data, 6) {
				time.Sleep(60 * time.Millisecond)
				_, _ = w.Write(chunk)
			}
			_ = w.Close()
		}()

		st, err := a.PutPart(t.Context(), attA, "zim", testToken, hexSHA(data), int64(len(data)), body)

		require.NoError(t, err)
		assert.Equal(t, libapi.StateStaged, st.State)
	})
}

// chunks splits b into n nearly equal chunks.
func chunks(b []byte, n int) [][]byte {
	var out [][]byte
	size := (len(b) + n - 1) / n
	for len(b) > 0 {
		k := min(size, len(b))
		out = append(out, b[:k])
		b = b[k:]
	}
	return out
}

// zeros is an endless reader of zero bytes.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestReserveAbortsWrite(t *testing.T) {
	t.Run("before writing", func(t *testing.T) {
		a, ts := newTestAttempts(t)
		a.Reserve = 100
		ts.Free = func(string) (int64, error) { return 149, nil }
		register(t, a, uploadReq(attA, "zim"))

		_, err := a.PutPart(t.Context(), attA, "zim", testToken, hexSHA(make([]byte, 50)), 50, bytes.NewReader(make([]byte, 50)))

		require.ErrorIs(t, err, ErrInsufficientSpace)
		assert.Equal(t, libapi.StateRegistered, ts.state(attA))
		assert.NoFileExists(t, ts.stagedFile(attA, ".zim.part"))
	})
	t.Run("free space drops mid-write", func(t *testing.T) {
		a, ts := newTestAttempts(t)
		a.Reserve = 100
		current := place(t, ts.DataDir, armFile, "small-b.zim")
		before := fixture(t, "small-b.zim")
		partial := ts.stagedFile(attA, ".zim.part")
		// The disk fills from elsewhere once the first re-check point is written.
		ts.Free = func(string) (int64, error) {
			if st, err := os.Stat(partial); err == nil && st.Size() >= spaceCheckEvery {
				return 0, nil
			}
			return 1 << 40, nil
		}
		register(t, a, uploadReq(attA, "zim"))
		length := int64(spaceCheckEvery + 1<<20)
		h := sha256.New()
		_, err := io.Copy(h, io.LimitReader(zeros{}, length))
		require.NoError(t, err)

		_, err = a.PutPart(t.Context(), attA, "zim", testToken, hex.EncodeToString(h.Sum(nil)), length, io.LimitReader(zeros{}, length))

		require.ErrorIs(t, err, ErrInsufficientSpace)
		assert.Equal(t, libapi.StateRegistered, ts.state(attA))
		assert.NoFileExists(t, partial)
		got, err := os.ReadFile(current)
		require.NoError(t, err)
		assert.Equal(t, before, got, "the current file is untouched")
	})
}

func TestGCRemovesExpiredAndOrphans(t *testing.T) {
	a, ts := newTestAttempts(t)
	expired := uploadReq(attA, "zim")
	expired.Deadline = clockA.Add(-time.Minute)
	register(t, a, expired)
	expiredPartial := ts.setReceiving(t, attA)
	for _, at := range []struct {
		id  string
		age time.Duration
	}{{attB, 8 * day}, {attC, 6 * day}} {
		register(t, a, uploadReq(at.id, "zim"))
		ts.Now = func() time.Time { return clockA.Add(-at.age) }
		require.NoError(t, a.Discard(at.id))
	}
	ts.Now = func() time.Time { return clockA }
	live := uploadReq(attD, "zim")
	live.Deadline = clockA.Add(time.Hour)
	register(t, a, live)
	liveFile := place(t, filepath.Dir(expiredPartial), attD+".zim", "small-a.zim")
	orphan := place(t, filepath.Dir(expiredPartial), "zzzzzzzz.zim.part", "small-b.zim")
	ts.terminal = nil

	removed, err := a.GC()

	require.NoError(t, err)
	assert.Equal(t, 2, removed)
	assert.Equal(t, libapi.StateDiscarded, ts.state(attA))
	assert.NoFileExists(t, expiredPartial)
	assert.Equal(t, []string{attA}, ts.terminal)
	assert.NoFileExists(t, orphan)
	assert.NotContains(t, ts.M.Attempts, attB)
	assert.Contains(t, ts.M.Attempts, attC)
	assert.FileExists(t, liveFile)
	assert.Equal(t, libapi.StateRegistered, ts.state(attD))
	saved, _, err := LoadManifest(filepath.Join(ts.DataDir, ".librarian"))
	require.NoError(t, err)
	assert.NotContains(t, saved.Attempts, attB)
	assert.Equal(t, libapi.StateDiscarded, saved.Attempts[attA].State)
}

func TestCompletePartRefusesTerminal(t *testing.T) {
	a, _ := newTestAttempts(t)
	register(t, a, fetchReq(attA))
	require.NoError(t, a.Discard(attA))

	err := a.CompletePart(attA, "zim", filepath.Join(fixturesDir, "small-a.zim"), "sha", 1)

	require.ErrorIs(t, err, ErrConflict)
	require.ErrorIs(t, a.CompletePart(attB, "zim", "x", "sha", 1), ErrNotFound)
}

func TestCompletePartStagesFetch(t *testing.T) {
	a, ts := newTestAttempts(t)
	register(t, a, fetchReq(attA))
	partial := ts.setReceiving(t, attA)
	data := fixture(t, "small-a.zim")
	require.NoError(t, os.WriteFile(partial, data, 0o600))

	require.NoError(t, a.CompletePart(attA, "zim", partial, hexSHA(data), int64(len(data))))

	st, err := a.Get(attA)
	require.NoError(t, err)
	assert.Equal(t, libapi.StateStaged, st.State)
	require.NotNil(t, st.Staged)
	assert.Equal(t, uuidA, st.Staged.BookID)
	assert.Equal(t, hexSHA(data), st.Staged.SHA256)
	assert.NoFileExists(t, partial)
	assert.FileExists(t, ts.stagedFile(attA, ".zim"))
	assert.Empty(t, ts.M.Attempts[attA].PartialPath)
}

func TestStagedBookSurvivesReload(t *testing.T) {
	a, ts := newTestAttempts(t)
	register(t, a, uploadReq(attA, "zim"))
	_, err := put(t, a, attA, "zim", fixture(t, "small-a.zim"))
	require.NoError(t, err)

	m, _, err := LoadManifest(filepath.Join(ts.DataDir, ".librarian"))
	require.NoError(t, err)
	fresh := &Attempts{S: reopenStore(t, ts.DataDir, m).Store}
	st, err := fresh.Get(attA)

	require.NoError(t, err)
	assert.Equal(t, libapi.StateStaged, st.State)
	require.NotNil(t, st.Staged)
	assert.Equal(t, uuidA, st.Staged.BookID)
}

func TestInvalidZimAfterLastPart(t *testing.T) {
	a, ts := newTestAttempts(t)
	register(t, a, uploadReq(attA, "zim", "warcs"))
	_, err := put(t, a, attA, "warcs", []byte("warc tar"))
	require.NoError(t, err)
	ts.R.(*fakeReader).fail[ts.stagedFile(attA, ".zim")] = errors.New("not a zim")

	_, err = put(t, a, attA, "zim", fixture(t, "small-a.zim"))

	require.ErrorIs(t, err, ErrInvalidZim)
	assert.Equal(t, libapi.StateFailed, ts.state(attA))
	assert.Equal(t, libapi.CodeInvalidZim, ts.M.Attempts[attA].Code)
	assert.NoFileExists(t, ts.stagedFile(attA, ".zim"))
	assert.NoFileExists(t, ts.stagedFile(attA, ".warcs"))
	assert.Equal(t, []string{attA}, ts.terminal)
}

func TestDiscardPromotingConflicts(t *testing.T) {
	a, ts := newTestAttempts(t)
	ts.stage(t, attA, armRef, "small-a.zim")
	rec := ts.M.Attempts[attA]
	rec.State = libapi.StatePromoting
	ts.M.Attempts[attA] = rec

	require.ErrorIs(t, a.Discard(attA), ErrConflict)
	assert.FileExists(t, ts.stagedFile(attA, ".zim"))

	register(t, a, uploadReq(attB, "zim"))
	require.NoError(t, a.Fail(attB, libapi.CodeInternal, "boom"))
	require.NoError(t, a.Discard(attB), "discarding a terminal attempt is a no-op")
	assert.Equal(t, libapi.StateFailed, ts.state(attB))
	assert.Equal(t, []string{attB}, ts.terminal)
}

func TestDiscardStagedRemovesFiles(t *testing.T) {
	a, ts := newTestAttempts(t)
	ts.stage(t, attA, armRef, "small-a.zim")

	require.NoError(t, a.Discard(attA))

	assert.Equal(t, libapi.StateDiscarded, ts.state(attA))
	assert.NoFileExists(t, ts.stagedFile(attA, ".zim"))
	assert.Equal(t, []string{attA}, ts.terminal)
	require.ErrorIs(t, a.Discard(attB), ErrNotFound)
}

func TestDiscardFailedSaveKeepsFilesAndState(t *testing.T) {
	a, ts := newTestAttempts(t)
	ts.stage(t, attA, armRef, "small-a.zim")
	dir := filepath.Join(ts.DataDir, ".librarian")
	require.NoError(t, ts.M.Save(dir))
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })

	require.Error(t, a.Discard(attA))

	assert.Equal(t, libapi.StateStaged, ts.state(attA))
	assert.FileExists(t, ts.stagedFile(attA, ".zim"))
	assert.Empty(t, ts.terminal, "nothing became terminal on disk")
}

func TestFailTerminalIsNoop(t *testing.T) {
	a, ts := newTestAttempts(t)
	register(t, a, fetchReq(attA))

	require.NoError(t, a.Fail(attA, libapi.CodeHashMismatch, "bad hash"))
	require.NoError(t, a.Fail(attA, libapi.CodeInternal, "later"))

	assert.Equal(t, libapi.StateFailed, ts.state(attA))
	assert.Equal(t, libapi.CodeHashMismatch, ts.M.Attempts[attA].Code)
	assert.Equal(t, "bad hash", ts.M.Attempts[attA].Error)
	assert.Equal(t, []string{attA}, ts.terminal)
	require.ErrorIs(t, a.Fail(attB, libapi.CodeInternal, "x"), ErrNotFound)
}

func TestPartPutWhileReceivingIsBusy(t *testing.T) {
	a, ts := newTestAttempts(t)
	a.IdleTimeout = time.Minute
	register(t, a, uploadReq(attA, "zim", "warcs"))
	data := fixture(t, "small-a.zim")
	body, w := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := a.PutPart(t.Context(), attA, "zim", testToken, hexSHA(data), int64(len(data)), body)
		done <- err
	}()
	// Returns once the stalled upload is reading, so the attempt is receiving.
	_, err := w.Write(data[:10])
	require.NoError(t, err)

	for _, part := range []string{"zim", "warcs"} {
		_, err = put(t, a, attA, part, data)
		require.ErrorIs(t, err, ErrBusy, part)
		assert.NotErrorIs(t, err, ErrConflict, part)
	}
	assert.Equal(t, libapi.StateReceiving, ts.state(attA))

	_, err = w.Write(data[10:])
	require.NoError(t, err)
	require.NoError(t, w.Close())
	require.NoError(t, <-done)
}

func TestDiscardDuringUpload(t *testing.T) {
	a, ts := newTestAttempts(t)
	register(t, a, uploadReq(attA, "zim"))
	data := fixture(t, "small-a.zim")
	body, w := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := a.PutPart(t.Context(), attA, "zim", testToken, hexSHA(data), int64(len(data)), body)
		done <- err
	}()
	_, err := w.Write(data[:10])
	require.NoError(t, err)

	require.NoError(t, a.Discard(attA))
	_, err = w.Write(data[10:])
	require.NoError(t, err)
	require.NoError(t, w.Close())

	require.ErrorIs(t, <-done, ErrConflict)
	assert.Equal(t, libapi.StateDiscarded, ts.state(attA))
	assert.NoFileExists(t, ts.stagedFile(attA, ".zim.part"))
	assert.NoFileExists(t, ts.stagedFile(attA, ".zim"))
}

func TestGetReportsCurrent(t *testing.T) {
	a, ts := newTestAttempts(t)
	ts.stage(t, attA, armRef, "small-a.zim")
	ts.promote(t, attA, "")
	register(t, a, uploadReq(attB, "zim"))

	st, err := a.Get(attB)

	require.NoError(t, err)
	require.NotNil(t, st.Current)
	assert.Equal(t, uuidA, st.Current.BookID)
	_, err = a.Get(attC)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestConcurrentPutAndGet(t *testing.T) {
	a, ts := newTestAttempts(t)
	ts.OnTerminal = nil
	ts.stage(t, attA, "other", "small-b.zim")
	data := fixture(t, "small-a.zim")
	ids := []string{"eeeeeeee", "ffffffff", "gggggggg", "hhhhhhhh"}
	for _, id := range ids {
		register(t, a, uploadReq(id, "zim"))
	}

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() {
			_, err := put(t, a, id, "zim", data)
			assert.NoError(t, err)
		})
		wg.Go(func() {
			for range 50 {
				_, err := a.Get(id)
				assert.NoError(t, err)
			}
		})
	}
	wg.Go(func() {
		_, err := a.Register(uploadReq("iiiiiiii", "zim"))
		assert.NoError(t, err)
	})
	wg.Go(func() {
		_, err := ts.Promote(t.Context(), attA, libapi.PromoteRequest{})
		assert.NoError(t, err)
	})
	wg.Wait()

	for _, id := range ids {
		assert.Equal(t, libapi.StateStaged, ts.state(id))
	}
	assert.Equal(t, libapi.StatePromoted, ts.state(attA))
}

func TestSnapshotFreeBytes(t *testing.T) {
	ts := newTestStore(t)
	ts.Free = func(string) (int64, error) { return 1234, nil }
	require.NoError(t, ts.Rescan(t.Context()))
	assert.Equal(t, int64(1234), ts.Snapshot().Library.FreeBytes)

	ts.Free = func(string) (int64, error) { return 0, errors.New("statfs failed") }
	require.NoError(t, ts.Rescan(t.Context()))
	assert.Zero(t, ts.Snapshot().Library.FreeBytes)
}
