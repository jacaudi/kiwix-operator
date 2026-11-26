package librarian

import (
	"bytes"
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jacaudi/kiwix-operator/internal/libapi"
)

const testUA = "kiwix-operator/test"

// recorder keeps the headers of every request a test server saw, the redirect hop included.
type recorder struct {
	mu   sync.Mutex
	reqs []http.Header
}

func (rec *recorder) add(r *http.Request) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.reqs = append(rec.reqs, r.Header.Clone())
}

func (rec *recorder) all() []http.Header {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]http.Header(nil), rec.reqs...)
}

// zimServer serves h at /a.zim behind a redirect from /start and returns the URL to fetch, which carries a token in its query.
func zimServer(t *testing.T, h http.HandlerFunc) (*recorder, string) {
	t.Helper()
	rec := &recorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		http.Redirect(w, r, "/a.zim", http.StatusFound)
	})
	mux.HandleFunc("/a.zim", func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		h(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return rec, srv.URL + "/start?token=secret"
}

// serveData answers with data, honoring Range as a real file server does.
func serveData(data []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "a.zim", time.Time{}, bytes.NewReader(data))
	}
}

// stall sends the headers, with length as Content-Length, and the first n bytes of data, then holds the response open until the client goes away or the test ends.
func stall(t *testing.T, length string, data []byte, n int) http.HandlerFunc {
	t.Helper()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", length)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data[:n])
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}
}

func newTestFetcher(t *testing.T) (*Fetcher, *testStore) {
	t.Helper()
	a, ts := newTestAttempts(t)
	return &Fetcher{A: a, UserAgent: testUA}, ts
}

func fetchFor(attempt, url, sha string, size int64) libapi.RegisterRequest {
	req := fetchReq(attempt)
	req.Fetch = &libapi.FetchSpec{URL: url, SHA256: sha, Size: size}
	return req
}

// waitDone blocks until attempt's Start goroutine has exited, failing the test if that takes longer than 5 s.
func waitDone(t *testing.T, f *Fetcher, attempt string) {
	t.Helper()
	f.mu.Lock()
	r := f.running[attempt]
	f.mu.Unlock()
	if r == nil {
		return
	}
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("fetch %s did not finish within 5 s", attempt)
	}
}

func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return st.Size()
}

func TestFetchFollowsRedirectAndStages(t *testing.T) {
	data := fixture(t, "small-a.zim")
	rec, url := zimServer(t, serveData(data))
	f, ts := newTestFetcher(t)
	register(t, f.A, fetchFor(attA, url, hexSHA(data), int64(len(data))))

	require.NoError(t, f.Run(t.Context(), attA))

	st, err := f.A.Get(attA)
	require.NoError(t, err)
	assert.Equal(t, libapi.StateStaged, st.State)
	require.NotNil(t, st.Staged)
	assert.Equal(t, uuidA, st.Staged.BookID)
	assert.Equal(t, hexSHA(data), st.Staged.SHA256)
	got, err := os.ReadFile(ts.stagedFile(attA, ".zim"))
	require.NoError(t, err)
	assert.Equal(t, data, got)
	assert.NoFileExists(t, ts.stagedFile(attA, ".zim.part"))
	reqs := rec.all()
	require.Len(t, reqs, 2, "the redirect hop and the download")
	for _, h := range reqs {
		assert.Equal(t, testUA, h.Get("User-Agent"))
		assert.Empty(t, h.Get("Range"), "no partial, so no Range")
	}
}

func TestFetchResumesWithRange(t *testing.T) {
	data := fixture(t, "small-a.zim")
	half := len(data) / 2
	rec, url := zimServer(t, serveData(data))
	f, ts := newTestFetcher(t)
	register(t, f.A, fetchFor(attA, url, hexSHA(data), int64(len(data))))
	partial := ts.setReceiving(t, attA)
	require.NoError(t, os.WriteFile(partial, data[:half], 0o600))

	require.NoError(t, f.Run(t.Context(), attA))

	assert.Equal(t, libapi.StateStaged, ts.state(attA))
	assert.Equal(t, hexSHA(data), ts.M.Attempts[attA].Parts[partZIM].SHA256)
	got, err := os.ReadFile(ts.stagedFile(attA, ".zim"))
	require.NoError(t, err)
	assert.Equal(t, data, got)
	reqs := rec.all()
	require.Len(t, reqs, 2)
	assert.Equal(t, "bytes="+strconv.Itoa(half)+"-", reqs[1].Get("Range"))
}

func TestFetchServerIgnoresRange(t *testing.T) {
	data := fixture(t, "small-a.zim")
	rec, url := zimServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(data)
	})
	f, ts := newTestFetcher(t)
	register(t, f.A, fetchFor(attA, url, hexSHA(data), int64(len(data))))
	partial := ts.setReceiving(t, attA)
	require.NoError(t, os.WriteFile(partial, bytes.Repeat([]byte{0xff}, len(data)/2), 0o600))

	require.NoError(t, f.Run(t.Context(), attA))

	assert.Equal(t, libapi.StateStaged, ts.state(attA))
	got, err := os.ReadFile(ts.stagedFile(attA, ".zim"))
	require.NoError(t, err)
	assert.Equal(t, data, got, "the kept bytes were dropped, not appended to")
	assert.NotEmpty(t, rec.all()[1].Get("Range"))
}

func TestFetchRestartsOnUnsatisfiableRange(t *testing.T) {
	data := fixture(t, "small-a.zim")
	rec, url := zimServer(t, serveData(data))
	f, ts := newTestFetcher(t)
	register(t, f.A, fetchFor(attA, url, hexSHA(data), 0))
	partial := ts.setReceiving(t, attA)
	require.NoError(t, os.WriteFile(partial, append(bytes.Clone(data), "trailing"...), 0o600))

	require.NoError(t, f.Run(t.Context(), attA))

	assert.Equal(t, libapi.StateStaged, ts.state(attA))
	got, err := os.ReadFile(ts.stagedFile(attA, ".zim"))
	require.NoError(t, err)
	assert.Equal(t, data, got)
	reqs := rec.all()
	require.Len(t, reqs, 4, "416, then one restart from zero")
	assert.NotEmpty(t, reqs[1].Get("Range"))
	assert.Empty(t, reqs[3].Get("Range"))
}

func TestFetchHTTPErrorHidesQuery(t *testing.T) {
	tests := []struct {
		name    string
		url     func(t *testing.T) string
		wantMsg string
	}{
		{"error status", func(t *testing.T) string {
			_, url := zimServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
			return url
		}, "404 Not Found from 127.0.0.1:"},
		{"transport error", func(t *testing.T) string {
			srv := httptest.NewServer(http.NotFoundHandler())
			srv.Close()
			return srv.URL + "/a.zim?token=secret"
		}, "127.0.0.1:"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, ts := newTestFetcher(t)
			register(t, f.A, fetchFor(attA, tc.url(t), "", 0))

			require.Error(t, f.Run(t.Context(), attA))

			rec := ts.M.Attempts[attA]
			assert.Equal(t, libapi.StateFailed, rec.State)
			assert.Equal(t, libapi.CodeInternal, rec.Code)
			assert.Contains(t, rec.Error, tc.wantMsg)
			assert.NotContains(t, rec.Error, "secret")
			assert.NotContains(t, rec.Error, "token")
		})
	}
}

func TestFetchHashMismatch(t *testing.T) {
	data := fixture(t, "small-a.zim")
	_, url := zimServer(t, serveData(data))
	f, ts := newTestFetcher(t)
	register(t, f.A, fetchFor(attA, url, hexSHA([]byte("other")), int64(len(data))))

	err := f.Run(t.Context(), attA)

	require.ErrorIs(t, err, ErrHashMismatch)
	rec := ts.M.Attempts[attA]
	assert.Equal(t, libapi.StateFailed, rec.State)
	assert.Equal(t, libapi.CodeHashMismatch, rec.Code)
	assert.NoFileExists(t, ts.stagedFile(attA, ".zim.part"))
	assert.NoFileExists(t, ts.stagedFile(attA, ".zim"))
	assert.Equal(t, []string{attA}, ts.terminal)
}

func TestFetchAbortsAtReserve(t *testing.T) {
	data := fixture(t, "small-a.zim")
	size := int64(len(data))
	tests := []struct {
		name          string
		declared      int64
		contentLength string
	}{
		{"declared size", size, strconv.FormatInt(size, 10)},
		{"content length when no size is declared", 0, strconv.FormatInt(size, 10)},
		{"hostile content length", 0, strconv.FormatInt(math.MaxInt64, 10)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The body never arrives, so reading it would end in the idle timeout, not InsufficientSpace.
			_, url := zimServer(t, stall(t, tc.contentLength, data, 0))
			f, ts := newTestFetcher(t)
			f.A.Reserve = 100
			ts.Free = func(string) (int64, error) { return size + 99, nil }
			register(t, f.A, fetchFor(attA, url, "", tc.declared))

			err := f.Run(t.Context(), attA)

			require.ErrorIs(t, err, ErrInsufficientSpace)
			rec := ts.M.Attempts[attA]
			assert.Equal(t, libapi.StateFailed, rec.State)
			assert.Equal(t, libapi.CodeInsufficientSpace, rec.Code)
			assert.NoFileExists(t, ts.stagedFile(attA, ".zim.part"))
		})
	}
	length := int64(spaceCheckEvery + 1<<20)
	for _, tc := range []struct {
		name     string
		declared int64
	}{
		{"free space drops mid-download", length},
		// No Size and no Content-Length, so the body is chunked and outruns its zero estimate at once.
		{"chunked body runs past the reserve", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, url := zimServer(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.Copy(w, io.LimitReader(zeros{}, length))
			})
			f, ts := newTestFetcher(t)
			f.A.Reserve = 100
			partial := ts.stagedFile(attA, ".zim.part")
			ts.Free = func(string) (int64, error) {
				if fileSize(partial) >= spaceCheckEvery {
					return 50, nil
				}
				return 1 << 40, nil
			}
			register(t, f.A, fetchFor(attA, url, "", tc.declared))

			err := f.Run(t.Context(), attA)

			require.ErrorIs(t, err, ErrInsufficientSpace)
			assert.Equal(t, libapi.CodeInsufficientSpace, ts.M.Attempts[attA].Code)
			assert.NoFileExists(t, partial)
		})
	}
}

func TestFetchChecksDeclaredSize(t *testing.T) {
	data := fixture(t, "small-a.zim")
	size := int64(len(data))
	tests := []struct {
		name     string
		declared int64
		// held keeps the response open after the body, so only a reader that stops at the declared size sees the end.
		held    bool
		wantMsg string
	}{
		{"longer body", size - 10, false, "received more than declared size " + strconv.FormatInt(size-10, 10)},
		{"longer body held open", size - 10, true, "received more than declared size " + strconv.FormatInt(size-10, 10)},
		{"shorter body", size + 10, false, "received " + strconv.FormatInt(size, 10) + " of " + strconv.FormatInt(size+10, 10) + " bytes"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := serveData(data)
			if tc.held {
				h = stall(t, strconv.FormatInt(size+1, 10), data, len(data))
			}
			_, url := zimServer(t, h)
			f, ts := newTestFetcher(t)
			register(t, f.A, fetchFor(attA, url, "", tc.declared))

			require.Error(t, f.Run(t.Context(), attA))

			rec := ts.M.Attempts[attA]
			assert.Equal(t, libapi.StateFailed, rec.State)
			assert.Equal(t, libapi.CodeInternal, rec.Code)
			assert.Contains(t, rec.Error, tc.wantMsg)
			assert.NoFileExists(t, ts.stagedFile(attA, ".zim.part"))
			assert.NoFileExists(t, ts.stagedFile(attA, ".zim"))
		})
	}
}

func TestFetchRejectsNegativeSize(t *testing.T) {
	data := fixture(t, "small-a.zim")
	_, url := zimServer(t, serveData(data))
	f, ts := newTestFetcher(t)
	register(t, f.A, fetchFor(attA, url, "", -1))

	require.Error(t, f.Run(t.Context(), attA))

	rec := ts.M.Attempts[attA]
	assert.Equal(t, libapi.StateFailed, rec.State)
	assert.Equal(t, libapi.CodeInternal, rec.Code)
}

func TestFetchRespectsSemaphore(t *testing.T) {
	t.Run("one download at a time", func(t *testing.T) {
		data := fixture(t, "small-a.zim")
		var mu sync.Mutex
		inFlight, most := 0, 0
		_, url := zimServer(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			inFlight++
			most = max(most, inFlight)
			mu.Unlock()
			// Long enough that a second download let through would overlap this one.
			time.Sleep(100 * time.Millisecond)
			serveData(data)(w, r)
			mu.Lock()
			inFlight--
			mu.Unlock()
		})
		f, ts := newTestFetcher(t)
		f.Sem = make(chan struct{}, 1)
		register(t, f.A, fetchFor(attA, url, hexSHA(data), 0))
		register(t, f.A, fetchFor(attB, url, hexSHA(data), 0))

		var wg sync.WaitGroup
		for _, id := range []string{attA, attB} {
			wg.Go(func() { assert.NoError(t, f.Run(t.Context(), id)) })
		}
		wg.Wait()

		mu.Lock()
		assert.Equal(t, 1, most)
		mu.Unlock()
		assert.Equal(t, libapi.StateStaged, ts.state(attA))
		assert.Equal(t, libapi.StateStaged, ts.state(attB))
	})
	t.Run("a waiting fetch leaves when cancelled", func(t *testing.T) {
		rec, url := zimServer(t, serveData(fixture(t, "small-a.zim")))
		f, ts := newTestFetcher(t)
		f.Sem = make(chan struct{}, 1)
		f.Sem <- struct{}{}
		register(t, f.A, fetchFor(attA, url, "", 0))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		err := f.Run(ctx, attA)

		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, libapi.StateRegistered, ts.state(attA))
		assert.Empty(t, rec.all())
	})
}

func TestStartIsIdempotent(t *testing.T) {
	data := fixture(t, "small-a.zim")
	var downloads atomic.Int32
	arrived, release := make(chan struct{}, 1), make(chan struct{})
	_, url := zimServer(t, func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		select {
		case arrived <- struct{}{}:
		default:
		}
		<-release
		serveData(data)(w, r)
	})
	f, ts := newTestFetcher(t)
	register(t, f.A, fetchFor(attA, url, hexSHA(data), 0))

	f.Start(attA)
	f.Start(attA)
	<-arrived
	f.Start(attA)
	close(release)
	waitDone(t, f, attA)

	assert.Equal(t, int32(1), downloads.Load())
	assert.Equal(t, libapi.StateStaged, ts.state(attA))
}

func TestCancelStopsDownload(t *testing.T) {
	data := fixture(t, "small-a.zim")
	half := len(data) / 2
	rec, url := zimServer(t, stall(t, strconv.Itoa(len(data)), data, half))
	f, ts := newTestFetcher(t)
	register(t, f.A, fetchFor(attA, url, hexSHA(data), int64(len(data))))
	partial := ts.stagedFile(attA, ".zim.part")

	f.Start(attA)
	require.Eventually(t, func() bool { return fileSize(partial) == int64(half) }, 5*time.Second, 5*time.Millisecond)
	f.Cancel(attA)

	f.mu.Lock()
	assert.Empty(t, f.running, "Cancel returned before the goroutine exited")
	f.mu.Unlock()
	assert.NoFileExists(t, partial)
	assert.NoFileExists(t, ts.stagedFile(attA, ".zim"))
	assert.Equal(t, libapi.StateReceiving, ts.state(attA), "a cancelled fetch neither completes nor fails")
	assert.Empty(t, ts.terminal)
	assert.Len(t, rec.all(), 2)
}

func TestDiscardCancelsFetch(t *testing.T) {
	data := fixture(t, "small-a.zim")
	half := len(data) / 2
	_, url := zimServer(t, stall(t, strconv.Itoa(len(data)), data, half))
	f, ts := newTestFetcher(t)
	ts.OnTerminal = f.Cancel
	register(t, f.A, fetchFor(attA, url, hexSHA(data), int64(len(data))))
	partial := ts.stagedFile(attA, ".zim.part")
	f.Start(attA)
	require.Eventually(t, func() bool { return fileSize(partial) == int64(half) }, 5*time.Second, 5*time.Millisecond)

	require.NoError(t, f.A.Discard(attA))

	f.mu.Lock()
	assert.Empty(t, f.running, "Discard returned before the fetch goroutine exited")
	f.mu.Unlock()
	rec := ts.M.Attempts[attA]
	assert.Equal(t, libapi.StateDiscarded, rec.State)
	assert.Empty(t, rec.Code, "the cancelled fetch did not call Fail")
	assert.NoFileExists(t, partial)
}

// TestCancelOnTerminalFromOwnFetch wires OnTerminal to Cancel, so a fetch that ends its own attempt reaches Cancel on its own goroutine.
func TestCancelOnTerminalFromOwnFetch(t *testing.T) {
	data := fixture(t, "small-a.zim")
	tests := []struct {
		name      string
		sha       string
		badZim    bool
		wantState string
		wantCode  string
	}{
		{"completes", hexSHA(data), false, libapi.StateStaged, ""},
		{"fails", hexSHA([]byte("other")), false, libapi.StateFailed, libapi.CodeHashMismatch},
		{"completes into an invalid ZIM", hexSHA(data), true, libapi.StateFailed, libapi.CodeInvalidZim},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, url := zimServer(t, serveData(data))
			f, ts := newTestFetcher(t)
			ts.OnTerminal = f.Cancel
			if tc.badZim {
				ts.R.(*fakeReader).fail[ts.stagedFile(attA, ".zim")] = assert.AnError
			}
			register(t, f.A, fetchFor(attA, url, tc.sha, 0))

			f.Start(attA)
			waitDone(t, f, attA)

			rec := ts.M.Attempts[attA]
			assert.Equal(t, tc.wantState, rec.State)
			assert.Equal(t, tc.wantCode, rec.Code)
			assert.NoFileExists(t, ts.stagedFile(attA, ".zim.part"))
		})
	}
}

func TestRunRefusesOtherAttempts(t *testing.T) {
	f, ts := newTestFetcher(t)
	register(t, f.A, uploadReq(attA, "zim"))
	ts.stage(t, attB, armRef, "small-a.zim")
	staged := ts.M.Attempts[attB]
	staged.Kind, staged.Fetch = libapi.KindFetch, &libapi.FetchSpec{URL: "https://example.org/a.zim"}
	ts.M.Attempts[attB] = staged

	require.ErrorIs(t, f.Run(t.Context(), attA), ErrConflict)
	require.ErrorIs(t, f.Run(t.Context(), attB), ErrConflict)
	require.ErrorIs(t, f.Run(t.Context(), attC), ErrNotFound)
	assert.Equal(t, libapi.StateRegistered, ts.state(attA))
	assert.Equal(t, libapi.StateStaged, ts.state(attB))
}
