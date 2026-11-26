package libclient

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jacaudi/kiwix-operator/internal/libapi"
)

const (
	adminToken  = "admin-token"
	uploadToken = "upload-token"
	goodSHA     = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// seen is what the fake librarian received on its last request.
type seen struct {
	method, path, query string
	header              http.Header
	body                string
	transferEncoding    []string
	contentLength       int64
}

// fixture serves one canned reply and records the request it got.
func fixture(t *testing.T, status int, reply string) (*Client, *seen) {
	t.Helper()
	got := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		*got = seen{
			method: r.Method, path: r.URL.EscapedPath(), query: r.URL.RawQuery, header: r.Header.Clone(),
			body: string(body), transferEncoding: r.TransferEncoding, contentLength: r.ContentLength,
		}
		if reply != "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, Token: adminToken}, got
}

func TestJSONMethods(t *testing.T) {
	book := libapi.BookInfo{Archive: "docs", File: "docs.zim", BookID: "11111111-1111-1111-1111-111111111111", ArticleCount: 7, SizeKiB: 9}
	state := libapi.AttemptState{Archive: "docs", Attempt: "abcdefg2", Kind: libapi.KindUpload, State: libapi.StateRegistered}
	lib := libapi.Library{Books: []libapi.BookInfo{book}, Unmanaged: []libapi.UnmanagedFile{{Name: "x.zim", Error: "bad"}}, FreeBytes: 42}
	marshal := func(v any) string {
		b, err := json.Marshal(v)
		require.NoError(t, err)
		return string(b)
	}

	tests := []struct {
		name       string
		status     int
		reply      string
		call       func(context.Context, *Client) (any, error)
		wantMethod string
		wantPath   string
		wantQuery  string
		wantBody   string
		want       any
	}{
		{
			name: "Register", status: 200, reply: marshal(state),
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Register(ctx, libapi.RegisterRequest{Archive: "docs", Attempt: "abcdefg2", Kind: libapi.KindUpload, TokenHash: "h"})
			},
			wantMethod: "POST", wantPath: "/v1/attempts",
			wantBody: `{"archive":"docs","attempt":"abcdefg2","kind":"upload","tokenHash":"h"}`, want: state,
		},
		{
			name: "Get", status: 200, reply: marshal(state),
			call:       func(ctx context.Context, c *Client) (any, error) { return c.Get(ctx, "abcdefg2") },
			wantMethod: "GET", wantPath: "/v1/attempts/abcdefg2", want: state,
		},
		{
			name: "Promote", status: 200, reply: marshal(book),
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Promote(ctx, "abcdefg2", libapi.PromoteRequest{ExpectedCurrentBookID: "old", SourceVersion: "2026-11", SourceFingerprint: "url:x"})
			},
			wantMethod: "POST", wantPath: "/v1/attempts/abcdefg2/promote",
			wantBody: `{"expectedCurrentBookID":"old","sourceVersion":"2026-11","sourceFingerprint":"url:x"}`, want: book,
		},
		{
			name: "Discard", status: 200, reply: marshal(state),
			call:       func(ctx context.Context, c *Client) (any, error) { return c.Discard(ctx, "abcdefg2") },
			wantMethod: "POST", wantPath: "/v1/attempts/abcdefg2/discard", want: state,
		},
		{
			name: "Archive", status: 200, reply: marshal(book),
			call:       func(ctx context.Context, c *Client) (any, error) { return c.Archive(ctx, "docs") },
			wantMethod: "GET", wantPath: "/v1/archives/docs", want: &book,
		},
		{
			name: "Adopt", status: 200, reply: marshal(book),
			call: func(ctx context.Context, c *Client) (any, error) {
				return c.Adopt(ctx, "docs", libapi.AdoptRequest{File: "docs.zim", ExpectedCurrentBookID: "old"})
			},
			wantMethod: "POST", wantPath: "/v1/archives/docs/adopt",
			wantBody: `{"file":"docs.zim","expectedCurrentBookID":"old"}`, want: book,
		},
		{
			name: "Delete", status: 200,
			call:       func(ctx context.Context, c *Client) (any, error) { return c.Delete(ctx, "docs", "Retain") },
			wantMethod: "DELETE", wantPath: "/v1/archives/docs", wantQuery: "policy=Retain", want: true,
		},
		{
			name: "Library", status: 200, reply: marshal(lib),
			call:       func(ctx context.Context, c *Client) (any, error) { return c.Library(ctx) },
			wantMethod: "GET", wantPath: "/v1/library", want: lib,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, got := fixture(t, tt.status, tt.reply)

			res, err := tt.call(t.Context(), c)

			require.NoError(t, err)
			assert.Equal(t, tt.want, res)
			assert.Equal(t, tt.wantMethod, got.method)
			assert.Equal(t, tt.wantPath, got.path)
			assert.Equal(t, tt.wantQuery, got.query)
			assert.Equal(t, "Bearer "+adminToken, got.header.Get("Authorization"))
			if tt.wantBody == "" {
				assert.Empty(t, got.body)
				return
			}
			assert.JSONEq(t, tt.wantBody, got.body)
			assert.Equal(t, "application/json", got.header.Get("Content-Type"))
		})
	}
}

func TestArchiveNotFoundIsNil(t *testing.T) {
	c, _ := fixture(t, http.StatusNotFound, `{"code":"NotFound","message":"archive docs has no current book"}`)

	info, err := c.Archive(t.Context(), "docs")

	require.NoError(t, err)
	assert.Nil(t, info)
}

func TestArchiveOtherErrorsStay(t *testing.T) {
	c, _ := fixture(t, http.StatusUnauthorized, `{"code":"Unauthorized","message":"nope"}`)

	info, err := c.Archive(t.Context(), "docs")

	require.ErrorIs(t, err, ErrUnauthorized)
	assert.Nil(t, info)
}

func TestDelete202(t *testing.T) {
	c, got := fixture(t, http.StatusAccepted, "")

	done, err := c.Delete(t.Context(), "docs", "Delete")

	require.NoError(t, err)
	assert.False(t, done)
	assert.Equal(t, "policy=Delete", got.query)
}

func TestPathSegmentsAreEscaped(t *testing.T) {
	c, got := fixture(t, http.StatusOK, `{}`)

	_, err := c.Get(t.Context(), "a/b?c")

	require.NoError(t, err)
	assert.Equal(t, "/v1/attempts/a%2Fb%3Fc", got.path)
	assert.Empty(t, got.query)
}

func TestPutPart(t *testing.T) {
	const payload = "zim bytes"
	tests := []struct {
		name   string
		body   string
		length int64
	}{
		{name: "streamed body", body: payload, length: int64(len(payload))},
		{name: "empty body", body: "", length: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, got := fixture(t, http.StatusOK, `{}`)
			// A reader net/http cannot size, so only the explicit length keeps the body from going chunked.
			body := io.MultiReader(iotest.OneByteReader(strings.NewReader(tt.body)))

			err := c.PutPart(t.Context(), "abcdefg2", "zim", uploadToken, goodSHA, tt.length, body)

			require.NoError(t, err)
			assert.Equal(t, "PUT", got.method)
			assert.Equal(t, "/v1/attempts/abcdefg2/parts/zim", got.path)
			assert.Equal(t, "Bearer "+uploadToken, got.header.Get("Authorization"))
			assert.Equal(t, goodSHA, got.header.Get("X-Content-SHA256"))
			assert.Equal(t, []string{strconv.FormatInt(tt.length, 10)}, got.header.Values("Content-Length"))
			assert.Equal(t, tt.length, got.contentLength)
			assert.Empty(t, got.transferEncoding)
			assert.Equal(t, tt.body, got.body)
		})
	}
}

func TestPutPartRejectsNegativeLength(t *testing.T) {
	c, got := fixture(t, http.StatusOK, `{}`)

	err := c.PutPart(t.Context(), "abcdefg2", "zim", uploadToken, goodSHA, -1, strings.NewReader("x"))

	require.Error(t, err)
	assert.Empty(t, got.method, "nothing may be sent")
}

func TestErrorMapping(t *testing.T) {
	// Mirrors the librarian's statusOf table; the sentinels are the contract the controllers and the uploader branch on.
	tests := []struct {
		code     string
		status   int
		sentinel error
	}{
		{libapi.CodeCurrentChanged, http.StatusConflict, ErrCurrentChanged},
		{libapi.CodeDuplicateBook, http.StatusConflict, ErrDuplicateBook},
		{libapi.CodeConflict, http.StatusConflict, ErrConflict},
		{libapi.CodeUnauthorized, http.StatusUnauthorized, ErrUnauthorized},
		{libapi.CodeNotFound, http.StatusNotFound, ErrNotFound},
		{libapi.CodeInsufficientSpace, http.StatusInsufficientStorage, ErrInsufficientSpace},
		{libapi.CodeHashMismatch, http.StatusUnprocessableEntity, ErrHashMismatch},
		{libapi.CodeAdoptFileMissing, http.StatusNotFound, ErrAdoptMissing},
		{libapi.CodeAdoptFileInvalid, http.StatusBadRequest, ErrAdoptInvalid},
		{libapi.CodeBusy, http.StatusServiceUnavailable, ErrBusy},
	}
	all := make([]error, 0, len(tests))
	for _, tt := range tests {
		all = append(all, tt.sentinel)
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			reply, err := json.Marshal(libapi.ErrorBody{Code: tt.code, Message: "server says " + tt.code})
			require.NoError(t, err)
			c, _ := fixture(t, tt.status, string(reply))

			_, got := c.Get(t.Context(), "abcdefg2")

			require.ErrorIs(t, got, tt.sentinel)
			assert.ErrorContains(t, got, "server says "+tt.code)
			for _, other := range all {
				if !errors.Is(other, tt.sentinel) {
					assert.NotErrorIs(t, got, other)
				}
			}
		})
	}
}

func TestUnmappedCodesWrapCodeAndMessage(t *testing.T) {
	tests := []struct {
		code   string
		status int
	}{
		{libapi.CodeInvalidZim, http.StatusUnprocessableEntity},
		{libapi.CodeInvalid, http.StatusBadRequest},
		{libapi.CodeInternal, http.StatusInternalServerError},
		{"SomethingNew", http.StatusTeapot},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			c, _ := fixture(t, tt.status, `{"code":"`+tt.code+`","message":"detail text"}`)

			_, err := c.Get(t.Context(), "abcdefg2")

			require.Error(t, err)
			assert.ErrorContains(t, err, tt.code)
			assert.ErrorContains(t, err, "detail text")
			for _, s := range []error{ErrBusy, ErrConflict, ErrNotFound, ErrUnauthorized, ErrHashMismatch, ErrInsufficientSpace} {
				assert.NotErrorIs(t, err, s)
			}
		})
	}
}

func TestNonJSONErrorBodyIsTruncated(t *testing.T) {
	tests := []struct {
		name   string
		reply  string
		wanted string
	}{
		{"proxy html", "<html>" + strings.Repeat("a", 600) + "TAIL</html>", strings.Repeat("a", 100)},
		{"json without a code", `{"detail":"` + strings.Repeat("b", 600) + `TAIL"}`, strings.Repeat("b", 100)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := fixture(t, http.StatusBadGateway, tt.reply)

			_, err := c.Get(t.Context(), "abcdefg2")

			require.Error(t, err)
			assert.ErrorContains(t, err, "502")
			assert.NotContains(t, err.Error(), "TAIL")
			assert.Contains(t, err.Error(), tt.wanted)
			assert.Less(t, len(err.Error()), 512+200)
		})
	}
}

func TestSuccessBodyIsCapped(t *testing.T) {
	huge := `{"freeBytes":1,"pad":"` + strings.Repeat("x", maxResponse) + `"}`
	c, _ := fixture(t, http.StatusOK, huge)

	_, err := c.Library(t.Context())

	require.Error(t, err)
	assert.ErrorContains(t, err, "response exceeds")
}

func TestUndecodableSuccessBodyIsAnError(t *testing.T) {
	c, _ := fixture(t, http.StatusOK, `<html>not json</html>`)

	_, err := c.Library(t.Context())

	require.Error(t, err)
}

func TestTransportErrorIsReturned(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := &Client{BaseURL: url, Token: adminToken}

	_, err := c.Library(t.Context())

	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrNotFound))
}

// TestTimeouts runs with the call timeouts shortened and a server slower than the short one, so each call shows which bound it carries. The injected client has no Timeout of its own: the bounds come from the per-call context.
func TestTimeouts(t *testing.T) {
	oldShort, oldLong := callTimeout, longCallTimeout
	callTimeout, longCallTimeout = 30*time.Millisecond, 5*time.Second
	t.Cleanup(func() { callTimeout, longCallTimeout = oldShort, oldLong })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(150 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, Token: adminToken, HTTP: &http.Client{}}

	tests := []struct {
		name      string
		call      func(context.Context) error
		wantTimed bool
	}{
		{"Register", func(ctx context.Context) error { _, err := c.Register(ctx, libapi.RegisterRequest{}); return err }, true},
		{"Get", func(ctx context.Context) error { _, err := c.Get(ctx, "a"); return err }, true},
		{"Discard", func(ctx context.Context) error { _, err := c.Discard(ctx, "a"); return err }, true},
		{"Archive", func(ctx context.Context) error { _, err := c.Archive(ctx, "a"); return err }, true},
		{"Library", func(ctx context.Context) error { _, err := c.Library(ctx); return err }, true},
		{"Promote", func(ctx context.Context) error { _, err := c.Promote(ctx, "a", libapi.PromoteRequest{}); return err }, false},
		{"Adopt", func(ctx context.Context) error { _, err := c.Adopt(ctx, "a", libapi.AdoptRequest{}); return err }, false},
		{"Delete", func(ctx context.Context) error { _, err := c.Delete(ctx, "a", "Retain"); return err }, false},
		{"PutPart", func(ctx context.Context) error {
			return c.PutPart(ctx, "a", "zim", uploadToken, goodSHA, 1, strings.NewReader("x"))
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call(t.Context())

			if tt.wantTimed {
				assert.ErrorIs(t, err, context.DeadlineExceeded)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestPutPartHonorsCallerContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// The body is read first: the server notices a vanished client only once it has.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	err := c.PutPart(ctx, "a", "zim", uploadToken, goodSHA, 1, strings.NewReader("x"))

	assert.ErrorIs(t, err, context.DeadlineExceeded)
}
