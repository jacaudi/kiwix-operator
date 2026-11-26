package upload

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jacaudi/kiwix-operator/internal/libapi"
	"github.com/jacaudi/kiwix-operator/internal/libclient"
)

const (
	testAttempt = "abcdefg2"
	testToken   = "upload-token"
)

// put is one PUT the fake librarian received.
type put struct {
	part, sha, auth, attempt string
	length                   int64
	body                     []byte
}

// reply is what the fake librarian answers one request with: a status and an error code, or a dropped connection.
type reply struct {
	status int
	code   string
	drop   bool
}

var ok = reply{status: http.StatusOK}

func busy() reply { return reply{status: http.StatusServiceUnavailable, code: libapi.CodeBusy} }

// fakeLibrarian answers the nth request (1-based) with script(n) and records every PUT it receives.
func fakeLibrarian(t *testing.T, script func(n int) reply) (*libclient.Client, func() []put) {
	t.Helper()
	var (
		mu   sync.Mutex
		puts []put
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The body is always drained first: a reply sent before the upload is read surfaces as a reset, not as the status.
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		mu.Lock()
		puts = append(puts, put{
			part: filepath.Base(r.URL.Path), sha: r.Header.Get("X-Content-SHA256"), auth: r.Header.Get("Authorization"),
			attempt: filepath.Base(filepath.Dir(filepath.Dir(r.URL.Path))), length: r.ContentLength, body: body,
		})
		n := len(puts)
		mu.Unlock()

		rep := script(n)
		if rep.drop {
			conn, _, err := w.(http.Hijacker).Hijack()
			if assert.NoError(t, err) {
				assert.NoError(t, conn.Close())
			}
			return
		}
		if rep.code != "" {
			b, err := json.Marshal(libapi.ErrorBody{Code: rep.code, Message: "server says " + rep.code})
			assert.NoError(t, err)
			w.WriteHeader(rep.status)
			_, _ = w.Write(b)
			return
		}
		w.WriteHeader(rep.status)
	}))
	t.Cleanup(srv.Close)
	return &libclient.Client{BaseURL: srv.URL}, func() []put {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(puts)
	}
}

// config returns a Config for dir whose Sleep records instead of waiting.
func config(dir string) (Config, *[]time.Duration) {
	var sleeps []time.Duration
	return Config{
		OutputDir: dir, Attempt: testAttempt, Token: testToken,
		Budget: 6 * time.Hour, MaxBackoff: 5 * time.Minute,
		Sleep: func(d time.Duration) { sleeps = append(sleeps, d) },
	}, &sleeps
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// seedCrawl lays out a finished crawl: one ZIM and two WARCs under an archive dir, plus the root-only profile dir the crawler leaves.
func seedCrawl(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs.zim"), "zim bytes")
	archive := filepath.Join(dir, "build", "collections", "crawl-a", "archive")
	writeFile(t, filepath.Join(archive, "rec-1.warc.gz"), "warc one")
	writeFile(t, filepath.Join(archive, "rec-0.warc.gz"), "warc zero")
	profile := filepath.Join(dir, "build", "collections", "crawl-a", "profile")
	writeFile(t, filepath.Join(profile, "Default", "Cookies"), "secret")
	require.NoError(t, os.Chmod(profile, 0o000))
	t.Cleanup(func() { _ = os.Chmod(profile, 0o755) })
	return dir
}

func tarNames(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	var names []string
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		require.NoError(t, err)
		names = append(names, h.Name)
	}
}

func TestUploadSingleZim(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs.zim"), "zim bytes")
	c, puts := fakeLibrarian(t, func(int) reply { return ok })
	cfg, sleeps := config(dir)

	require.NoError(t, Run(t.Context(), cfg, c))

	got := puts()
	require.Len(t, got, 1)
	assert.Equal(t, put{
		part: "zim", sha: sum([]byte("zim bytes")), auth: "Bearer " + testToken, attempt: testAttempt,
		length: int64(len("zim bytes")), body: []byte("zim bytes"),
	}, got[0])
	assert.Empty(t, *sleeps)
	assert.NoFileExists(t, filepath.Join(dir, "warcs.tar"), "no tar without KeepWarcs")
}

func TestUploadRequiresExactlyOneZim(t *testing.T) {
	tests := []struct {
		name    string
		files   []string
		dirs    []string
		wantErr string
	}{
		{name: "none", wantErr: "found 0 .zim files"},
		{name: "two", files: []string{"a.zim", "b.zim"}, wantErr: "found 2 .zim files"},
		{name: "a directory named like a zim is not a zim", dirs: []string{"x.zim"}, wantErr: "found 0 .zim files"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tt.files {
				writeFile(t, filepath.Join(dir, f), "z")
			}
			for _, d := range tt.dirs {
				require.NoError(t, os.Mkdir(filepath.Join(dir, d), 0o755))
			}
			c, puts := fakeLibrarian(t, func(int) reply { return ok })
			cfg, _ := config(dir)

			err := Run(t.Context(), cfg, c)

			require.ErrorContains(t, err, tt.wantErr)
			assert.Empty(t, puts(), "nothing may be sent")
		})
	}
}

func TestUploadWarcsTarOnce(t *testing.T) {
	dir := seedCrawl(t)
	// Every warcs PUT but the third is refused, so the part is sent three times.
	c, puts := fakeLibrarian(t, func(n int) reply {
		if n == 2 || n == 3 {
			return busy()
		}
		return ok
	})
	cfg, sleeps := config(dir)
	cfg.KeepWarcs = true

	require.NoError(t, Run(t.Context(), cfg, c))

	got := puts()
	require.Len(t, got, 4)
	assert.Equal(t, []string{"zim", "warcs", "warcs", "warcs"}, []string{got[0].part, got[1].part, got[2].part, got[3].part})
	tarBytes, err := os.ReadFile(filepath.Join(dir, "warcs.tar"))
	require.NoError(t, err)
	for _, p := range got[1:] {
		assert.Equal(t, sum(tarBytes), p.sha, "retries resend the same tar")
		assert.Equal(t, tarBytes, p.body)
		assert.Equal(t, int64(len(tarBytes)), p.length)
	}
	assert.Equal(t, []time.Duration{5 * time.Second, 10 * time.Second}, *sleeps)
	assert.NoFileExists(t, filepath.Join(dir, "warcs.tar.tmp"))
}

func TestUploadWarcsTarHoldsOnlyArchiveDirsSorted(t *testing.T) {
	dir := seedCrawl(t)
	other := filepath.Join(dir, "build", "collections", "crawl-0", "archive")
	writeFile(t, filepath.Join(other, "rec-0.warc.gz"), "other")
	c, _ := fakeLibrarian(t, func(int) reply { return ok })
	cfg, _ := config(dir)
	cfg.KeepWarcs = true

	require.NoError(t, Run(t.Context(), cfg, c))

	assert.Equal(t, []string{
		"build/collections/crawl-0/archive/",
		"build/collections/crawl-0/archive/rec-0.warc.gz",
		"build/collections/crawl-a/archive/",
		"build/collections/crawl-a/archive/rec-0.warc.gz",
		"build/collections/crawl-a/archive/rec-1.warc.gz",
	}, tarNames(t, filepath.Join(dir, "warcs.tar")))
}

func TestUploadSkipsExistingWarcsTar(t *testing.T) {
	dir := seedCrawl(t)
	writeFile(t, filepath.Join(dir, "warcs.tar"), "built earlier")
	c, puts := fakeLibrarian(t, func(int) reply { return ok })
	cfg, _ := config(dir)
	cfg.KeepWarcs = true

	require.NoError(t, Run(t.Context(), cfg, c))

	got := puts()
	require.Len(t, got, 2)
	assert.Equal(t, "warcs", got[1].part)
	assert.Equal(t, []byte("built earlier"), got[1].body)
}

func TestUploadWarcsNeedsAnArchiveDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs.zim"), "z")
	c, puts := fakeLibrarian(t, func(int) reply { return ok })
	cfg, _ := config(dir)
	cfg.KeepWarcs = true

	err := Run(t.Context(), cfg, c)

	require.ErrorContains(t, err, "no WARC archive")
	assert.Empty(t, puts(), "the failure comes before any upload")
	assert.NoFileExists(t, filepath.Join(dir, "warcs.tar"))
	assert.NoFileExists(t, filepath.Join(dir, "warcs.tar.tmp"))
}

func TestUploadRetriesWithCappedBackoff(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs.zim"), "z")
	c, puts := fakeLibrarian(t, func(n int) reply {
		if n <= 6 {
			return busy()
		}
		return ok
	})
	cfg, sleeps := config(dir)
	cfg.MaxBackoff = 60 * time.Second

	require.NoError(t, Run(t.Context(), cfg, c))

	assert.Equal(t, []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second, 60 * time.Second}, *sleeps)
	assert.Len(t, puts(), 7)
}

func TestUploadGivesUpAtBudget(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs.zim"), "z")
	c, puts := fakeLibrarian(t, func(int) reply { return busy() })
	cfg, sleeps := config(dir)
	cfg.Budget = time.Minute
	cfg.MaxBackoff = 60 * time.Second

	err := Run(t.Context(), cfg, c)

	require.ErrorIs(t, err, libclient.ErrBusy)
	assert.ErrorContains(t, err, "gave up")
	// 5s + 10s + 20s slept; the next 40s would take the total to 75s, past the 1m budget.
	assert.Equal(t, []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second}, *sleeps)
	assert.Len(t, puts(), 4)
}

func TestUploadRetryClassification(t *testing.T) {
	tests := []struct {
		name      string
		first     reply
		wantRetry bool
	}{
		{name: "busy", first: busy(), wantRetry: true},
		{name: "internal", first: reply{status: http.StatusInternalServerError, code: libapi.CodeInternal}, wantRetry: true},
		{name: "proxy 502 without a code", first: reply{status: http.StatusBadGateway}, wantRetry: true},
		{name: "dropped connection", first: reply{drop: true}, wantRetry: true},
		{name: "conflict", first: reply{status: http.StatusConflict, code: libapi.CodeConflict}},
		{name: "unauthorized", first: reply{status: http.StatusUnauthorized, code: libapi.CodeUnauthorized}},
		{name: "hash mismatch", first: reply{status: http.StatusUnprocessableEntity, code: libapi.CodeHashMismatch}},
		{name: "insufficient space", first: reply{status: http.StatusInsufficientStorage, code: libapi.CodeInsufficientSpace}},
		{name: "not found", first: reply{status: http.StatusNotFound, code: libapi.CodeNotFound}},
		{name: "invalid", first: reply{status: http.StatusBadRequest, code: libapi.CodeInvalid}},
		{name: "invalid zim", first: reply{status: http.StatusUnprocessableEntity, code: libapi.CodeInvalidZim}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "docs.zim"), "z")
			c, puts := fakeLibrarian(t, func(n int) reply {
				if n == 1 {
					return tt.first
				}
				return ok
			})
			cfg, sleeps := config(dir)

			err := Run(t.Context(), cfg, c)

			if tt.wantRetry {
				require.NoError(t, err)
				assert.Len(t, puts(), 2)
				assert.Equal(t, []time.Duration{5 * time.Second}, *sleeps)
				return
			}
			require.Error(t, err)
			assert.Len(t, puts(), 1, "a final answer is not retried")
			assert.Empty(t, *sleeps)
		})
	}
}

func TestUploadNoRetryOnConflict(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs.zim"), "z")
	c, puts := fakeLibrarian(t, func(int) reply { return reply{status: http.StatusConflict, code: libapi.CodeConflict} })
	cfg, _ := config(dir)

	err := Run(t.Context(), cfg, c)

	require.ErrorIs(t, err, libclient.ErrConflict)
	assert.Len(t, puts(), 1)
}

func TestUploadStopsWhenContextEndsDuringBackoff(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs.zim"), "z")
	c, puts := fakeLibrarian(t, func(int) reply { return busy() })
	cfg, _ := config(dir)
	ctx, cancel := context.WithCancel(t.Context())
	cfg.Sleep = func(time.Duration) { cancel() }

	err := Run(ctx, cfg, c)

	require.ErrorIs(t, err, context.Canceled)
	assert.Len(t, puts(), 1, "no PUT after the context ended")
}

func TestUploadDefaultSleepEndsWithContext(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "docs.zim"), "z")
	c, _ := fakeLibrarian(t, func(int) reply { return busy() })
	cfg, _ := config(dir)
	cfg.Sleep = nil
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := Run(ctx, cfg, c)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 3*time.Second, "the 5s backoff must not outlast the context")
}

func TestConfigValidate(t *testing.T) {
	good := Config{OutputDir: "/output", Attempt: testAttempt, Token: testToken, Budget: time.Hour, MaxBackoff: time.Minute}
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "valid", mutate: func(*Config) {}},
		{name: "zero budget means one try", mutate: func(c *Config) { c.Budget = 0 }},
		{name: "no output dir", mutate: func(c *Config) { c.OutputDir = "" }, wantErr: "OutputDir"},
		{name: "no attempt", mutate: func(c *Config) { c.Attempt = "" }, wantErr: "Attempt"},
		{name: "no token", mutate: func(c *Config) { c.Token = "" }, wantErr: "Token"},
		{name: "negative budget", mutate: func(c *Config) { c.Budget = -time.Second }, wantErr: "Budget"},
		{name: "no max backoff", mutate: func(c *Config) { c.MaxBackoff = 0 }, wantErr: "MaxBackoff"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := good
			tt.mutate(&cfg)

			err := cfg.Validate()

			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}
