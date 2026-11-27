//go:build kiwix

package librarian

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jacaudi/kiwix-operator/internal/libapi"
)

// recoveryDone is the log message Run writes once its background startup recovery has finished.
const recoveryDone = "startup recovery finished"

// syncBuffer is a bytes.Buffer safe for the logger's writes and the test's reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

// testLibrarian is one Run of the librarian over dir, confirming against kiwix-serve on kiwixPort.
type testLibrarian struct {
	dir      string
	base     string
	kiwixURL string
	logs     *syncBuffer
	cancel   context.CancelFunc
	done     chan error
	stopped  bool
}

func librarianConfig(t *testing.T, dir string, kiwixPort int, logs io.Writer) Config {
	t.Helper()
	return Config{
		DataDir:        dir,
		Listen:         fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		KiwixServeURL:  fmt.Sprintf("http://127.0.0.1:%d", kiwixPort),
		AdminToken:     adminToken,
		KiwixManageBin: "kiwix-manage",
		MaxFetches:     1,
		UserAgent:      "kiwix-operator/test",
		// The test sources listen on loopback, which fetchClient refuses.
		FetchHTTP: &http.Client{},
		Logger:    slog.New(slog.NewTextHandler(io.MultiWriter(logs, t.Output()), &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

// startLibrarian runs the librarian and returns once its startup probe answers 200.
func startLibrarian(t *testing.T, dir string, kiwixPort int) *testLibrarian {
	t.Helper()
	logs := &syncBuffer{}
	cfg := librarianConfig(t, dir, kiwixPort, logs)
	ctx, cancel := context.WithCancel(context.Background())
	l := &testLibrarian{dir: dir, base: "http://" + cfg.Listen, kiwixURL: cfg.KiwixServeURL, logs: logs, cancel: cancel, done: make(chan error, 1)}
	go func() { l.done <- Run(ctx, cfg) }()
	t.Cleanup(func() { l.stop(t) })

	deadline := time.Now().Add(60 * time.Second)
	for {
		select {
		case err := <-l.done:
			l.stopped = true
			t.Fatalf("Run returned before the startup probe passed: %v", err)
		default:
		}
		if status, _ := get(l.base + "/healthz/startup"); status == http.StatusOK {
			return l
		}
		require.True(t, time.Now().Before(deadline), "startup probe did not pass within 60 s")
		time.Sleep(20 * time.Millisecond)
	}
}

// stop cancels Run and requires a clean return.
func (l *testLibrarian) stop(t *testing.T) {
	t.Helper()
	if l.stopped {
		return
	}
	l.stopped = true
	l.cancel()
	select {
	case err := <-l.done:
		require.NoError(t, err)
	case <-time.After(60 * time.Second):
		t.Fatal("Run did not return within 60 s of its context ending")
	}
}

func (l *testLibrarian) waitRecovered(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool { return strings.Contains(l.logs.String(), recoveryDone) }, 120*time.Second, 50*time.Millisecond)
}

// call sends an admin API request with body encoded as JSON, unless body is nil.
func (l *testLibrarian) call(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		require.NoError(t, err)
	}
	status, resp, err := l.try(method, path, data)
	require.NoError(t, err)
	return status, resp
}

// try is call without the test: it is safe in Eventually conditions and other goroutines.
func (l *testLibrarian) try(method, path string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(context.Background(), method, l.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	return sendReq(req)
}

func send(t *testing.T, req *http.Request) (int, []byte) {
	t.Helper()
	status, body, err := sendReq(req)
	require.NoError(t, err)
	return status, body
}

func sendReq(req *http.Request) (int, []byte, error) {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

// stageUpload registers an upload attempt for archive and PUTs fixture as its zim part; the attempt is then staged.
func (l *testLibrarian) stageUpload(t *testing.T, attempt, archive, fixtureFile string) {
	t.Helper()
	status, body := l.call(t, http.MethodPost, "/v1/attempts", libapi.RegisterRequest{
		Archive: archive, Attempt: attempt, Kind: libapi.KindUpload, TokenHash: hexSHA([]byte(testToken)), Parts: []string{"zim"},
	})
	require.Equal(t, http.StatusOK, status, string(body))

	data := fixture(t, fixtureFile)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, l.base+"/v1/attempts/"+attempt+"/parts/zim", bytes.NewReader(data))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("X-Content-SHA256", hexSHA(data))
	status, body = send(t, req)
	require.Equal(t, http.StatusOK, status, string(body))
	var st libapi.AttemptState
	require.NoError(t, json.Unmarshal(body, &st))
	require.Equal(t, libapi.StateStaged, st.State, string(body))
}

func (l *testLibrarian) promote(t *testing.T, attempt, expected string) libapi.BookInfo {
	t.Helper()
	status, body := l.call(t, http.MethodPost, "/v1/attempts/"+attempt+"/promote", libapi.PromoteRequest{
		ExpectedCurrentBookID: expected, SourceVersion: "2026-11", SourceFingerprint: "url:u",
	})
	require.Equal(t, http.StatusOK, status, string(body))
	var info libapi.BookInfo
	require.NoError(t, json.Unmarshal(body, &info))
	return info
}

func (l *testLibrarian) library(t *testing.T) libapi.Library {
	t.Helper()
	status, body := l.call(t, http.MethodGet, "/v1/library", nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var lib libapi.Library
	require.NoError(t, json.Unmarshal(body, &lib))
	return lib
}

// state returns the attempt's state, or "" when it cannot be read.
func (l *testLibrarian) state(attempt string) string {
	status, body, err := l.try(http.MethodGet, "/v1/attempts/"+attempt, nil)
	var st libapi.AttemptState
	if err != nil || status != http.StatusOK || json.Unmarshal(body, &st) != nil {
		return ""
	}
	return st.State
}

// startKiwixServe runs kiwix-serve on dir's library.xml, as the kiwix pod does, without waiting for it to answer.
func startKiwixServe(t *testing.T, dir string, port int) {
	t.Helper()
	cmd := exec.Command("kiwix-serve", "--monitorLibrary", "--address=127.0.0.1", "--port="+strconv.Itoa(port),
		"--attachToProcess="+strconv.Itoa(os.Getpid()), "--library", filepath.Join(dir, "library.xml"))
	cmd.Stdout, cmd.Stderr = t.Output(), t.Output()
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
}

func waitKiwixServe(t *testing.T, kiwixURL string) {
	t.Helper()
	require.Eventually(t, func() bool {
		status, _ := get(kiwixURL + "/catalog/v2/entries?count=-1")
		return status == http.StatusOK
	}, 30*time.Second, 50*time.Millisecond, "kiwix-serve did not start")
}

// get returns the status and body of a GET, or 0 when the request fails; it never follows redirects.
func get(url string) (int, string) {
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(url)
	if err != nil {
		return 0, ""
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func entry(kiwixURL, id string) (int, string) { return get(kiwixURL + "/catalog/v2/entry/" + id) }

func catalogCount(t *testing.T, kiwixURL string) int {
	t.Helper()
	status, body := get(kiwixURL + "/catalog/v2/entries?count=-1")
	require.Equal(t, http.StatusOK, status)
	return strings.Count(body, "<entry>")
}

func bookCount(t *testing.T, dir string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "library.xml"))
	require.NoError(t, err)
	return strings.Count(string(data), "<book ")
}

func uuidOf(t *testing.T, file string) string {
	t.Helper()
	return fixtureMeta(t, file).UUID
}

func TestWriteRoutineConfirmsWithSixteenBooks(t *testing.T) {
	dir, kport := t.TempDir(), freePort(t)
	for i := 1; i <= 15; i++ {
		place(t, dir, fmt.Sprintf("book-%02d.zim", i), fmt.Sprintf("book-%02d.zim", i))
	}
	l := startLibrarian(t, dir, kport)
	startKiwixServe(t, dir, kport)
	waitKiwixServe(t, l.kiwixURL)
	l.waitRecovered(t)
	require.Equal(t, 15, catalogCount(t, l.kiwixURL))
	l.stageUpload(t, attA, "book-16", "book-16.zim")

	info := l.promote(t, attA, "")

	assert.Equal(t, uuidOf(t, "book-16.zim"), info.BookID)
	assert.False(t, l.library(t).ReloadUnconfirmed, "the write was confirmed")
	assert.Equal(t, 16, catalogCount(t, l.kiwixURL))
	status, body := entry(l.kiwixURL, info.BookID)
	assert.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, `href="/content/book-16"`)
}

func TestTwoWritesInOneSecondBothConfirmed(t *testing.T) {
	dir, kport := t.TempDir(), freePort(t)
	l := startLibrarian(t, dir, kport)
	startKiwixServe(t, dir, kport)
	waitKiwixServe(t, l.kiwixURL)
	l.waitRecovered(t)
	l.stageUpload(t, attA, "arm-a", "small-a.zim")
	l.stageUpload(t, attB, "arm-b", "small-b.zim")

	promoteBody, err := json.Marshal(libapi.PromoteRequest{SourceVersion: "2026-11", SourceFingerprint: "url:u"})
	require.NoError(t, err)
	type result struct {
		status int
		body   []byte
		err    error
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	start := time.Now()
	for i, attempt := range []string{attA, attB} {
		wg.Go(func() {
			status, resp, err := l.try(http.MethodPost, "/v1/attempts/"+attempt+"/promote", promoteBody)
			results[i] = result{status, resp, err}
		})
	}
	wg.Wait()

	infos := make([]libapi.BookInfo, 2)
	for i, r := range results {
		require.NoError(t, r.err)
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		require.NoError(t, json.Unmarshal(r.body, &infos[i]))
	}
	assert.Less(t, time.Since(start), 30*time.Second, "both confirmed within the first window")
	assert.False(t, l.library(t).ReloadUnconfirmed)
	for i, want := range []struct{ id, stem string }{{uuidA, "arm-a"}, {uuidB, "arm-b"}} {
		assert.Equal(t, want.id, infos[i].BookID)
		status, body := entry(l.kiwixURL, want.id)
		assert.Equal(t, http.StatusOK, status)
		assert.Contains(t, body, `href="/content/`+want.stem+`"`)
	}
}

func TestWriteBetweenLoadAndBaselineRecovers(t *testing.T) {
	dir, kport := t.TempDir(), freePort(t)
	l := startLibrarian(t, dir, kport)
	l.waitRecovered(t)
	l.stageUpload(t, attA, armRef, "small-a.zim")

	startKiwixServe(t, dir, kport)
	info := l.promote(t, attA, "")

	assert.Equal(t, uuidA, info.BookID)
	assert.False(t, l.library(t).ReloadUnconfirmed, "confirmed within the render limit")
	status, body := entry(l.kiwixURL, uuidA)
	assert.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, `href="/content/`+armRef+`"`)
}

func TestAdoptConfirmedByContentLink(t *testing.T) {
	dir, kport := t.TempDir(), freePort(t)
	place(t, dir, "bg3.wiki_x.zim", "small-a.zim")
	l := startLibrarian(t, dir, kport)
	startKiwixServe(t, dir, kport)
	waitKiwixServe(t, l.kiwixURL)
	l.waitRecovered(t)
	status, _ := get(l.kiwixURL + "/content/bg3.wiki_x")
	require.NotEqual(t, http.StatusNotFound, status, "the unmanaged file is served under its own name first")

	status, body := l.call(t, http.MethodPost, "/v1/archives/bg3-wiki/adopt", libapi.AdoptRequest{File: "bg3.wiki_x.zim"})

	require.Equal(t, http.StatusOK, status, string(body))
	assert.False(t, l.library(t).ReloadUnconfirmed)
	status, catalog := entry(l.kiwixURL, uuidA)
	assert.Equal(t, http.StatusOK, status)
	assert.Contains(t, catalog, `href="/content/bg3-wiki"`)
	status, _ = get(l.kiwixURL + "/content/bg3-wiki")
	assert.NotEqual(t, http.StatusNotFound, status)
	status, _ = get(l.kiwixURL + "/content/bg3.wiki_x")
	assert.Equal(t, http.StatusNotFound, status, "the old link is gone")
	assert.NoFileExists(t, filepath.Join(dir, "bg3.wiki_x.zim"))
	assert.FileExists(t, filepath.Join(dir, "bg3-wiki.zim"))
}

func TestMalformedLibraryKeepsServing(t *testing.T) {
	dir, kport := t.TempDir(), freePort(t)
	place(t, dir, "arm-a.zim", "small-a.zim")
	l := startLibrarian(t, dir, kport)
	startKiwixServe(t, dir, kport)
	waitKiwixServe(t, l.kiwixURL)
	l.waitRecovered(t)
	status, _ := entry(l.kiwixURL, uuidA)
	require.Equal(t, http.StatusOK, status)
	path := filepath.Join(dir, "library.xml")
	st, err := os.Stat(path)
	require.NoError(t, err)

	mtime := st.ModTime().Truncate(time.Second).Add(time.Second)
	require.NoError(t, WriteFileAtomic(path, []byte("<library><book"), &mtime))
	// kiwix-serve checks the library's mtime once a second.
	time.Sleep(3 * time.Second)

	status, _ = entry(l.kiwixURL, uuidA)
	assert.Equal(t, http.StatusOK, status, "the books loaded before the malformed write are still served")
}

func TestRestartKeepsUnmanagedBooks(t *testing.T) {
	dir, kport := t.TempDir(), freePort(t)
	place(t, dir, "devdocs_en_x_2026-10.zim", "small-a.zim")
	l := startLibrarian(t, dir, kport)
	startKiwixServe(t, dir, kport)
	waitKiwixServe(t, l.kiwixURL)
	l.waitRecovered(t)
	status, _ := entry(l.kiwixURL, uuidA)
	require.Equal(t, http.StatusOK, status)
	l.stop(t)
	before, err := os.Stat(filepath.Join(dir, "library.xml"))
	require.NoError(t, err)

	l = startLibrarian(t, dir, kport)
	l.waitRecovered(t)

	after, err := os.Stat(filepath.Join(dir, "library.xml"))
	require.NoError(t, err)
	assert.True(t, after.ModTime().After(before.ModTime()), "the restart rendered library.xml")
	// Past kiwix-serve's one-second check of the startup render.
	time.Sleep(2 * time.Second)
	status, body := entry(l.kiwixURL, uuidA)
	assert.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, `href="/content/devdocs_en_x_2026-10"`)
}

func TestFetchResumesAfterRestart(t *testing.T) {
	data := fixture(t, "small-a.zim")
	half := len(data) / 2
	var ranges []string
	var mu sync.Mutex
	var requests atomic.Int32
	src := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data[:half])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		http.ServeContent(w, r, "a.zim", time.Time{}, bytes.NewReader(data))
	})
	srv := &http.Server{Handler: src, ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	dir, kport := t.TempDir(), freePort(t)
	l := startLibrarian(t, dir, kport)
	status, body := l.call(t, http.MethodPost, "/v1/attempts", libapi.RegisterRequest{
		Archive: armRef, Attempt: attA, Kind: libapi.KindFetch, TokenHash: hexSHA([]byte(testToken)),
		Fetch: &libapi.FetchSpec{URL: "http://" + ln.Addr().String() + "/a.zim", SHA256: hexSHA(data), Size: int64(len(data))},
	})
	require.Equal(t, http.StatusOK, status, string(body))
	partial := filepath.Join(dir, ".librarian", "staged", attA+".zim.part")
	require.Eventually(t, func() bool { return fileSize(partial) == int64(half) }, 30*time.Second, 20*time.Millisecond)

	l.stop(t)
	assert.Equal(t, int64(half), fileSize(partial), "shutdown keeps the partial")

	l = startLibrarian(t, dir, kport)
	require.Eventually(t, func() bool { return l.state(attA) == libapi.StateStaged }, 60*time.Second, 50*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"", "bytes=" + strconv.Itoa(half) + "-"}, ranges)
}

func TestSecondLibrarianRefused(t *testing.T) {
	dir, kport := t.TempDir(), freePort(t)
	startLibrarian(t, dir, kport)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	err := Run(ctx, librarianConfig(t, dir, kport, io.Discard))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "lock")
	assert.NoError(t, ctx.Err(), "refused at once, not at the timeout")
}

func TestFirstStartScansBeforeReady(t *testing.T) {
	dir, kport := t.TempDir(), freePort(t)
	place(t, dir, "arm-a.zim", "small-a.zim")
	place(t, dir, "Other Book.zim", "small-b.zim")
	place(t, dir, "tiny_2026-10.zim", "tiny.zim")

	l := startLibrarian(t, dir, kport)

	assert.Equal(t, 3, bookCount(t, dir), "library.xml has every book when the startup probe passes")
	startKiwixServe(t, dir, kport)
	waitKiwixServe(t, l.kiwixURL)
	assert.Equal(t, 3, catalogCount(t, l.kiwixURL))
}

func TestZeroBookStart(t *testing.T) {
	dir, kport := t.TempDir(), freePort(t)

	l := startLibrarian(t, dir, kport)

	assert.Equal(t, 0, bookCount(t, dir))
	startKiwixServe(t, dir, kport)
	waitKiwixServe(t, l.kiwixURL)
	assert.Equal(t, 0, catalogCount(t, l.kiwixURL))
}

func TestRefreshRenameOverServesNewID(t *testing.T) {
	dir, kport := t.TempDir(), freePort(t)
	l := startLibrarian(t, dir, kport)
	startKiwixServe(t, dir, kport)
	waitKiwixServe(t, l.kiwixURL)
	l.waitRecovered(t)
	l.stageUpload(t, attA, armRef, "small-a.zim")
	l.promote(t, attA, "")
	l.stageUpload(t, attB, armRef, "small-b.zim")

	info := l.promote(t, attB, uuidA)

	assert.Equal(t, uuidB, info.BookID)
	assert.False(t, l.library(t).ReloadUnconfirmed)
	status, body := entry(l.kiwixURL, uuidB)
	assert.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, `href="/content/`+armRef+`"`)
	status, _ = entry(l.kiwixURL, uuidA)
	assert.Equal(t, http.StatusNotFound, status)
}

func TestDeleteUnlinksAfterConfirmReal(t *testing.T) {
	dir, kport := t.TempDir(), freePort(t)
	l := startLibrarian(t, dir, kport)
	startKiwixServe(t, dir, kport)
	waitKiwixServe(t, l.kiwixURL)
	l.waitRecovered(t)
	l.stageUpload(t, attA, armRef, "small-a.zim")
	l.promote(t, attA, "")
	file := filepath.Join(dir, armFile)

	// The file is checked before the entry, so a missing file with the entry still served proves the unlink came first.
	var early atomic.Bool
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := os.Stat(file); err != nil {
				if status, _ := entry(l.kiwixURL, uuidA); status == http.StatusOK {
					early.Store(true)
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	status, body := l.call(t, http.MethodDelete, "/v1/archives/"+armRef+"?policy=Delete", nil)
	close(stop)
	wg.Wait()

	require.Equal(t, http.StatusOK, status, string(body))
	assert.False(t, early.Load(), "the file was unlinked while kiwix-serve still served its entry")
	assert.NoFileExists(t, file)
	status, _ = entry(l.kiwixURL, uuidA)
	assert.Equal(t, http.StatusNotFound, status)
}

func TestStartupNotBlockedByRecovery(t *testing.T) {
	dir, kport := t.TempDir(), freePort(t)
	stateDir := filepath.Join(dir, ".librarian")
	require.NoError(t, os.MkdirAll(stateDir, 0o750))
	target := place(t, dir, "arm-a.zim", "small-a.zim")
	place(t, dir, "extra_2026-10.zim", "small-b.zim")
	blocker := filepath.Join(stateDir, "blocker")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))
	book, err := KiwixManage{Bin: "kiwix-manage", ScratchDir: t.TempDir()}.Read(t.Context(), target)
	require.NoError(t, err)
	m := newManifest()
	m.Intents = []Intent{
		// Source gone and the target holds the new book: resumes at the confirmed write, which waits for kiwix-serve.
		{Op: opPromote, Archive: "arm-a", Source: filepath.Join(stateDir, "staged", "gone.zim"), NewBookID: uuidA, Book: book,
			Current: ArchiveCurrent{File: "arm-a.zim", BookID: uuidA, ArticleCount: book.ArticleCount, SizeKiB: book.SizeKiB}},
		// Its source cannot even be stat'ed, so roll-forward fails on it.
		{Op: opPromote, Archive: "arm-b", Source: filepath.Join(blocker, "x.zim"), NewBookID: uuidB, Book: Book{BookID: uuidB}},
	}
	require.NoError(t, m.Save(stateDir))

	l := startLibrarian(t, dir, kport)

	status, _ := l.call(t, http.MethodGet, "/v1/archives/arm-a", nil)
	assert.Equal(t, http.StatusNotFound, status, "the roll-forward is still waiting for kiwix-serve")
	onDisk, _, err := LoadManifest(stateDir)
	require.NoError(t, err)
	assert.Len(t, onDisk.Intents, 2, "the startup probe passed before roll-forward finished")

	startKiwixServe(t, dir, kport)
	l.waitRecovered(t)
	status, body := l.call(t, http.MethodGet, "/v1/archives/arm-a", nil)
	require.Equal(t, http.StatusOK, status, string(body))
	assert.Contains(t, string(body), uuidA)
	assert.Contains(t, l.logs.String(), "roll forward")
	assert.Contains(t, l.logs.String(), "arm-b")
	files := make([]string, 0, 2)
	for _, b := range l.library(t).Books {
		files = append(files, b.File)
	}
	assert.Contains(t, files, "extra_2026-10.zim", "the rescan ran after roll-forward failed")
}

func TestUnmanagedUppercaseConfirms(t *testing.T) {
	dir, kport := t.TempDir(), freePort(t)
	l := startLibrarian(t, dir, kport)
	l.waitRecovered(t)
	l.stop(t)
	place(t, dir, "Foo Bar.zim", "small-a.zim")
	startKiwixServe(t, dir, kport)
	waitKiwixServe(t, fmt.Sprintf("http://127.0.0.1:%d", kport))

	l = startLibrarian(t, dir, kport)
	l.waitRecovered(t)

	lib := l.library(t)
	require.Len(t, lib.Books, 1)
	assert.Equal(t, "Foo Bar.zim", lib.Books[0].File)
	assert.False(t, lib.ReloadUnconfirmed, "the rescan confirmed the book by its content link")
	status, body := entry(l.kiwixURL, uuidA)
	assert.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, `href="/content/foo_bar"`)
}
