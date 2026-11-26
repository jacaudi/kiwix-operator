package notify

import (
	"context"
	"encoding/json/v2"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// received is what the fake webhook saw on one request.
type received struct {
	method string
	header http.Header
	body   string
}

// hook answers with the given statuses in order, repeating the last, and records every request.
type hook struct {
	srv   *httptest.Server
	mu    sync.Mutex
	got   []received
	conns atomic.Int32
}

func newHook(t *testing.T, statuses ...int) *hook {
	t.Helper()
	h := &hook{}
	h.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		h.mu.Lock()
		h.got = append(h.got, received{method: r.Method, header: r.Header.Clone(), body: string(body)})
		status := statuses[min(len(h.got), len(statuses))-1]
		h.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, strings.Repeat("x", 1024))
	}))
	h.srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			h.conns.Add(1)
		}
	}
	h.srv.Start()
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hook) requests() []received {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]received(nil), h.got...)
}

// recorder returns a Sleep that records its waits instead of waiting.
func recorder() (func(time.Duration), *[]time.Duration) {
	var waits []time.Duration
	return func(d time.Duration) { waits = append(waits, d) }, &waits
}

func TestSendPayload(t *testing.T) {
	h := newHook(t, http.StatusNoContent)
	n := &Notifier{UserAgent: "kiwix-operator/test"}

	require.NoError(t, n.Send(t.Context(), h.srv.URL, "t", "m"))

	got := h.requests()
	require.Len(t, got, 1)
	assert.Equal(t, http.MethodPost, got[0].method)
	assert.Equal(t, `{"title":"t","message":"m"}`, got[0].body)
	assert.Equal(t, "application/json", got[0].header.Get("Content-Type"))
	assert.Equal(t, "kiwix-operator/test", got[0].header.Get("User-Agent"))
}

func TestSendInvalidUTF8(t *testing.T) {
	h := newHook(t, http.StatusOK)

	require.NoError(t, (&Notifier{}).Send(t.Context(), h.srv.URL, "t", "bad \xff byte"))

	got := h.requests()
	require.Len(t, got, 1)
	var sent payload
	require.NoError(t, json.Unmarshal([]byte(got[0].body), &sent))
	assert.Equal(t, "bad � byte", sent.Message)
}

func TestSendRetries(t *testing.T) {
	h := newHook(t, 500, 500, 204)
	sleep, waits := recorder()
	n := &Notifier{Retries: 3, Sleep: sleep}

	require.NoError(t, n.Send(t.Context(), h.srv.URL, "t", "m"))

	assert.Len(t, h.requests(), 3)
	assert.Equal(t, []time.Duration{time.Second, 2 * time.Second}, *waits)
}

func TestSendGivesUp(t *testing.T) {
	tests := []struct {
		name      string
		retries   int
		wantReqs  int
		wantWaits []time.Duration
	}{
		{"no retries", 0, 1, nil},
		{"three retries", 3, 4, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}},
		{"waits cap at four seconds", 5, 6, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second, 4 * time.Second}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHook(t, 500)
			sleep, waits := recorder()
			n := &Notifier{Retries: tt.retries, Sleep: sleep}

			err := n.Send(t.Context(), h.srv.URL, "t", "m")

			require.Error(t, err)
			assert.Contains(t, err.Error(), "500")
			assert.Len(t, h.requests(), tt.wantReqs)
			assert.Equal(t, tt.wantWaits, *waits)
		})
	}
}

func TestSendDrainsBodiesForConnectionReuse(t *testing.T) {
	h := newHook(t, 500, 500, 204)
	sleep, _ := recorder()
	n := &Notifier{Retries: 3, Sleep: sleep}

	require.NoError(t, n.Send(t.Context(), h.srv.URL, "t", "m"))

	assert.Len(t, h.requests(), 3)
	assert.EqualValues(t, 1, h.conns.Load())
}

func TestSendStopsWhenContextCancelled(t *testing.T) {
	h := newHook(t, 500)
	ctx, cancel := context.WithCancel(t.Context())
	n := &Notifier{Retries: 3, Sleep: func(time.Duration) { cancel() }}

	err := n.Send(ctx, h.srv.URL, "t", "m")

	require.ErrorIs(t, err, context.Canceled)
	assert.Len(t, h.requests(), 1)
}

func TestDefaultSleepReturnsWhenContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	n := &Notifier{Retries: 3}

	start := time.Now()
	err := n.Send(ctx, srv.URL, "t", "m")

	require.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), 500*time.Millisecond)
}

func TestSendTimesOutWithInjectedClient(t *testing.T) {
	old := requestTimeout
	requestTimeout = 50 * time.Millisecond
	t.Cleanup(func() { requestTimeout = old })
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// The server only notices a client disconnect once the body is read.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	n := &Notifier{HTTP: &http.Client{}}

	err := n.Send(t.Context(), srv.URL+"/hook?token=SECRET", "t", "m")

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotContains(t, err.Error(), "SECRET")
}

func TestSendErrorsHideWebhookURL(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	downURL := down.URL
	down.Close()
	bad := newHook(t, 500)

	tests := []struct {
		name string
		url  string
	}{
		{"non-2xx status", bad.srv.URL + "/hook/path?token=SECRET"},
		{"connection refused", downURL + "/hook/path?token=SECRET"},
		{"unparseable URL", "http://host/%zzSECRET?token=SECRET"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sleep, _ := recorder()
			n := &Notifier{Retries: 1, Sleep: sleep}

			err := n.Send(t.Context(), tt.url, "t", "m")

			require.Error(t, err)
			for _, leak := range []string{"SECRET", "/hook", "token"} {
				assert.NotContains(t, err.Error(), leak)
			}
		})
	}
}

func TestSendErrorNamesHost(t *testing.T) {
	h := newHook(t, 500)

	err := (&Notifier{}).Send(t.Context(), h.srv.URL+"/hook?token=SECRET", "t", "m")

	require.Error(t, err)
	assert.Contains(t, err.Error(), strings.TrimPrefix(h.srv.URL, "http://"))
}

func TestUpdatedMessage(t *testing.T) {
	tests := []struct {
		name                    string
		articles, prev, sizeKiB int64
		first                   bool
		wantTitle, wantMessage  string
	}{
		{"update", 3540, 3531, 49152, false, "docs updated", "3,540 pages, 48 MB (was 3,531)"},
		{"first version", 3540, 0, 49152, true, "docs updated", "3,540 pages, 48 MB (first version)"},
		{"size rounds down", 12, 10, 1535, false, "docs updated", "12 pages, 1 MB (was 10)"},
		{"millions", 1234567, 1000000, 2048, false, "docs updated", "1,234,567 pages, 2 MB (was 1,000,000)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			title, message := Updated("docs", tt.articles, tt.prev, tt.sizeKiB, tt.first)

			assert.Equal(t, tt.wantTitle, title)
			assert.Equal(t, tt.wantMessage, message)
		})
	}
}

func TestOtherMessages(t *testing.T) {
	tests := []struct {
		name                   string
		build                  func() (string, string)
		wantTitle, wantMessage string
	}{
		{"rejected", func() (string, string) { return Rejected("docs", "kept current") }, "docs refresh rejected", "kept current"},
		{"failed", func() (string, string) { return Failed("docs", "crawl deadline exceeded") }, "docs failed", "crawl deadline exceeded"},
		{
			"adopt missing", func() (string, string) { return AdoptMissing("docs", "old.zim") },
			"docs waiting for adoptFile", "old.zim not found on the library volume; remove adoptFile to fetch instead",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			title, message := tt.build()

			assert.Equal(t, tt.wantTitle, title)
			assert.Equal(t, tt.wantMessage, message)
		})
	}
}

func TestSendTotalTimeIsBounded(t *testing.T) {
	old := sendTimeout
	sendTimeout = 150 * time.Millisecond
	t.Cleanup(func() { sendTimeout = old })
	stall := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(stall.Close)
	sleep, _ := recorder()
	// Each request alone may take requestTimeout, and Retries multiplies it; the total must still end at sendTimeout.
	n := &Notifier{Retries: 3, Sleep: sleep}

	start := time.Now()
	err := n.Send(t.Context(), stall.URL, "t", "m")

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 5*time.Second)
}
