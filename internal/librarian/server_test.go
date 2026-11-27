package librarian

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jacaudi/kiwix-operator/internal/libapi"
)

const (
	adminToken = "admin-token"
	goodSHA    = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// fakeStore records each call and answers with err, or with the canned values.
type fakeStore struct {
	current map[string]*libapi.BookInfo
	err     error
	done    bool
	calls   []string
	promote libapi.PromoteRequest
	adopt   libapi.AdoptRequest
}

func (f *fakeStore) Current(archive string) *libapi.BookInfo { return f.current[archive] }

func (f *fakeStore) Promote(_ context.Context, attempt string, req libapi.PromoteRequest) (libapi.BookInfo, error) {
	f.calls = append(f.calls, "promote "+attempt)
	f.promote = req
	return libapi.BookInfo{Archive: armRef, BookID: uuidB}, f.err
}

func (f *fakeStore) Adopt(_ context.Context, archive string, req libapi.AdoptRequest) (libapi.BookInfo, error) {
	f.calls = append(f.calls, "adopt "+archive)
	f.adopt = req
	return libapi.BookInfo{Archive: archive, BookID: uuidA}, f.err
}

func (f *fakeStore) Delete(_ context.Context, archive, policy string) (bool, error) {
	f.calls = append(f.calls, "delete "+archive+" "+policy)
	return f.done, f.err
}

// fakeAttempts records each call; PutPart reads the body only when readBody is set.
type fakeAttempts struct {
	err      error
	calls    []string
	register libapi.RegisterRequest
	token    string
	sha      string
	length   int64
	readBody bool
	body     []byte
	readErr  error
	readDone chan struct{}
}

func (f *fakeAttempts) state(attempt string) libapi.AttemptState {
	return libapi.AttemptState{Archive: armRef, Attempt: attempt, Kind: libapi.KindUpload, State: libapi.StateRegistered}
}

func (f *fakeAttempts) Register(req libapi.RegisterRequest) (libapi.AttemptState, error) {
	f.calls = append(f.calls, "register "+req.Attempt)
	f.register = req
	return f.state(req.Attempt), f.err
}

func (f *fakeAttempts) PutPart(_ context.Context, attempt, part, token, sha string, length int64, body io.Reader) (libapi.AttemptState, error) {
	f.calls = append(f.calls, "put "+attempt+" "+part)
	f.token, f.sha, f.length = token, sha, length
	if f.readBody {
		f.body, f.readErr = io.ReadAll(body)
		close(f.readDone)
	}
	return f.state(attempt), f.err
}

func (f *fakeAttempts) Get(attempt string) (libapi.AttemptState, error) {
	f.calls = append(f.calls, "get "+attempt)
	return f.state(attempt), f.err
}

func (f *fakeAttempts) Discard(attempt string) error {
	f.calls = append(f.calls, "discard "+attempt)
	return f.err
}

type testHandler struct {
	http.Handler
	st      *fakeStore
	at      *fakeAttempts
	started bool
	warcs   map[string]string
}

func newTestHandler(t testing.TB) *testHandler {
	t.Helper()
	th := &testHandler{st: &fakeStore{current: map[string]*libapi.BookInfo{}}, at: &fakeAttempts{}, started: true, warcs: map[string]string{}}
	lib := func() libapi.Library { return libapi.Library{FreeBytes: 7} }
	warcs := func(archive string) (io.ReadCloser, error) {
		data, ok := th.warcs[archive]
		if !ok {
			return nil, fmt.Errorf("archive %s: %w", archive, ErrNotFound)
		}
		return io.NopCloser(strings.NewReader(data)), nil
	}
	th.Handler = NewHandler(th.st, th.at, adminToken, lib, warcs, func() bool { return th.started })
	return th
}

// do sends one request with the admin token unless hdr sets Authorization.
func do(h http.Handler, method, target, body string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+adminToken)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func errorBody(t *testing.T, w *httptest.ResponseRecorder) libapi.ErrorBody {
	t.Helper()
	var eb libapi.ErrorBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &eb), "body %q", w.Body.String())
	return eb
}

func TestRejectsBadPathSegments(t *testing.T) {
	tests := []struct{ method, path string }{
		{http.MethodGet, "/v1/attempts/ABC"},
		{http.MethodGet, "/v1/attempts/a%2Fb"},
		{http.MethodGet, "/v1/attempts/a%2fb"},
		{http.MethodPost, "/v1/archives/../x/adopt"},
		{http.MethodGet, "/v1/archives/./x"},
		{http.MethodGet, "/v1//library"},
		{http.MethodGet, "/v1/library/"},
		{http.MethodGet, "/v1/archives/arm_ref"},
		{http.MethodGet, "/v1/attempts/%61aaaaaaa"},
		{http.MethodGet, "/v1/archives/-arm"},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			th := newTestHandler(t)

			w := do(th, tc.method, tc.path, "")

			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Equal(t, libapi.CodeInvalid, errorBody(t, w).Code)
			assert.Empty(t, th.st.calls)
			assert.Empty(t, th.at.calls)
		})
	}
}

func TestAdminAuth(t *testing.T) {
	routes := []struct{ method, path string }{
		{http.MethodPost, "/v1/attempts"},
		{http.MethodGet, "/v1/attempts/" + attA},
		{http.MethodPost, "/v1/attempts/" + attA + "/promote"},
		{http.MethodPost, "/v1/attempts/" + attA + "/discard"},
		{http.MethodGet, "/v1/archives/" + armRef},
		{http.MethodPost, "/v1/archives/" + armRef + "/adopt"},
		{http.MethodDelete, "/v1/archives/" + armRef + "?policy=Retain"},
		{http.MethodGet, "/v1/archives/" + armRef + "/warcs"},
		{http.MethodGet, "/v1/library"},
	}
	auths := []struct{ name, header string }{
		{"missing", ""},
		{"wrong token", "Bearer nope"},
		{"empty token", "Bearer "},
		{"other scheme", "Basic " + adminToken},
		{"upload token", "Bearer " + testToken},
	}
	for _, rt := range routes {
		for _, a := range auths {
			t.Run(rt.method+" "+rt.path+" "+a.name, func(t *testing.T) {
				th := newTestHandler(t)

				w := do(th, rt.method, rt.path, "{}", "Authorization", a.header)

				assert.Equal(t, http.StatusUnauthorized, w.Code)
				assert.Equal(t, libapi.CodeUnauthorized, errorBody(t, w).Code)
				assert.Empty(t, th.st.calls)
				assert.Empty(t, th.at.calls)
			})
		}
	}
}

func TestEmptyAdminTokenRejectsAll(t *testing.T) {
	h := NewHandler(&fakeStore{}, &fakeAttempts{}, "", func() libapi.Library { return libapi.Library{} }, nil, func() bool { return true })

	for _, header := range []string{"Bearer ", "Bearer anything"} {
		w := do(h, http.MethodGet, "/v1/library", "", "Authorization", header)

		assert.Equal(t, http.StatusUnauthorized, w.Code, header)
	}
}

func TestPartPutHeaders(t *testing.T) {
	tests := []struct {
		name       string
		edit       func(r *http.Request)
		wantStatus int
		wantCode   string
	}{
		{"missing hash", func(r *http.Request) { r.Header.Del("X-Content-SHA256") }, http.StatusBadRequest, libapi.CodeInvalid},
		{"upper-case hash", func(r *http.Request) { r.Header.Set("X-Content-SHA256", strings.ToUpper(goodSHA)) }, http.StatusBadRequest, libapi.CodeInvalid},
		{"short hash", func(r *http.Request) { r.Header.Set("X-Content-SHA256", goodSHA[1:]) }, http.StatusBadRequest, libapi.CodeInvalid},
		{"hash with suffix", func(r *http.Request) { r.Header.Set("X-Content-SHA256", goodSHA+"\n") }, http.StatusBadRequest, libapi.CodeInvalid},
		{"chunked body", func(r *http.Request) { r.ContentLength = -1; r.Header.Del("Content-Length") }, http.StatusBadRequest, libapi.CodeInvalid},
		{"no token", func(r *http.Request) { r.Header.Del("Authorization") }, http.StatusUnauthorized, libapi.CodeUnauthorized},
		{"unknown part", func(r *http.Request) { r.URL.Path = "/v1/attempts/" + attA + "/parts/meta" }, http.StatusBadRequest, libapi.CodeInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			th := newTestHandler(t)
			r := httptest.NewRequest(http.MethodPut, "/v1/attempts/"+attA+"/parts/zim", strings.NewReader("data"))
			r.Header.Set("Authorization", "Bearer "+testToken)
			r.Header.Set("X-Content-SHA256", goodSHA)
			r.Header.Set("Content-Length", "4")
			tc.edit(r)
			w := httptest.NewRecorder()

			th.ServeHTTP(w, r)

			assert.Equal(t, tc.wantStatus, w.Code)
			assert.Equal(t, tc.wantCode, errorBody(t, w).Code)
			assert.Empty(t, th.at.calls)
		})
	}
}

// TestPartPutRequiresContentLengthHeader sends a PUT with neither Content-Length nor Transfer-Encoding over a real connection, where Go's server reports ContentLength 0, not -1.
func TestPartPutRequiresContentLengthHeader(t *testing.T) {
	th := newTestHandler(t)
	srv := httptest.NewServer(th)
	t.Cleanup(srv.Close)
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	_, err = fmt.Fprintf(conn, "PUT /v1/attempts/%s/parts/zim HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nX-Content-SHA256: %s\r\n\r\n", attA, testToken, goodSHA)
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	var eb libapi.ErrorBody
	require.NoError(t, json.UnmarshalRead(resp.Body, &eb))

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, libapi.CodeInvalid, eb.Code)
	assert.Empty(t, th.at.calls)
}

func TestPartPutPassesUploadToken(t *testing.T) {
	th := newTestHandler(t)

	w := do(th, http.MethodPut, "/v1/attempts/"+attA+"/parts/warcs", "data", "Authorization", "Bearer "+testToken, "X-Content-SHA256", goodSHA, "Content-Length", "4")

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, []string{"put " + attA + " warcs"}, th.at.calls)
	assert.Equal(t, testToken, th.at.token)
	assert.Equal(t, goodSHA, th.at.sha)
	assert.Equal(t, int64(4), th.at.length)
	var st libapi.AttemptState
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &st))
	assert.Equal(t, attA, st.Attempt)
}

func TestGetArchive(t *testing.T) {
	th := newTestHandler(t)

	w := do(th, http.MethodGet, "/v1/archives/"+armRef, "")
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, libapi.CodeNotFound, errorBody(t, w).Code)

	want := libapi.BookInfo{Archive: armRef, File: armFile, BookID: uuidA, ArticleCount: 3, SizeKiB: 1}
	th.st.current[armRef] = &want
	w = do(th, http.MethodGet, "/v1/archives/"+armRef, "")
	require.Equal(t, http.StatusOK, w.Code)
	var got libapi.BookInfo
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, want, got)
}

func TestDeletePendingIs202(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		done       bool
		wantStatus int
		wantCall   string
	}{
		{"pending", "?policy=Delete", false, http.StatusAccepted, "delete " + armRef + " Delete"},
		{"done", "?policy=Delete", true, http.StatusOK, "delete " + armRef + " Delete"},
		{"retain", "?policy=Retain", true, http.StatusOK, "delete " + armRef + " Retain"},
		{"no policy", "", true, http.StatusBadRequest, ""},
		{"unknown policy", "?policy=Purge", true, http.StatusBadRequest, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			th := newTestHandler(t)
			th.st.done = tc.done

			w := do(th, http.MethodDelete, "/v1/archives/"+armRef+tc.query, "")

			assert.Equal(t, tc.wantStatus, w.Code)
			if tc.wantCall == "" {
				assert.Equal(t, libapi.CodeInvalid, errorBody(t, w).Code)
				assert.Empty(t, th.st.calls)
				return
			}
			assert.Equal(t, []string{tc.wantCall}, th.st.calls)
		})
	}
}

func TestStatusMapping(t *testing.T) {
	tests := []struct {
		err        error
		wantStatus int
		wantCode   string
	}{
		{ErrInvalid, http.StatusBadRequest, libapi.CodeInvalid},
		{ErrAdoptInvalid, http.StatusBadRequest, libapi.CodeAdoptFileInvalid},
		{ErrUnauthorized, http.StatusUnauthorized, libapi.CodeUnauthorized},
		{ErrNotFound, http.StatusNotFound, libapi.CodeNotFound},
		{ErrAdoptMissing, http.StatusNotFound, libapi.CodeAdoptFileMissing},
		{ErrConflict, http.StatusConflict, libapi.CodeConflict},
		{ErrCurrentChanged, http.StatusConflict, libapi.CodeCurrentChanged},
		{ErrDuplicateBook, http.StatusConflict, libapi.CodeDuplicateBook},
		{ErrHashMismatch, http.StatusUnprocessableEntity, libapi.CodeHashMismatch},
		{ErrInvalidZim, http.StatusUnprocessableEntity, libapi.CodeInvalidZim},
		{ErrBusy, http.StatusServiceUnavailable, libapi.CodeBusy},
		{ErrInsufficientSpace, http.StatusInsufficientStorage, libapi.CodeInsufficientSpace},
		{errors.New("disk on fire"), http.StatusInternalServerError, libapi.CodeInternal},
	}
	for _, tc := range tests {
		t.Run(tc.wantCode+" "+tc.err.Error(), func(t *testing.T) {
			th := newTestHandler(t)
			th.st.err = fmt.Errorf("attempt %s: %w", attA, tc.err)

			w := do(th, http.MethodPost, "/v1/attempts/"+attA+"/promote", "{}")

			assert.Equal(t, tc.wantStatus, w.Code)
			eb := errorBody(t, w)
			assert.Equal(t, tc.wantCode, eb.Code)
			assert.Contains(t, eb.Message, tc.err.Error())
			if tc.wantCode == libapi.CodeBusy {
				assert.Equal(t, "5", w.Header().Get("Retry-After"))
			} else {
				assert.Empty(t, w.Header().Get("Retry-After"))
			}
		})
	}
}

func TestRoutesDispatch(t *testing.T) {
	tests := []struct {
		name, method, path, body string
		wantStoreCall            string
		wantAttemptCall          string
		check                    func(t *testing.T, th *testHandler, body []byte)
	}{
		{
			name: "register", method: http.MethodPost, path: "/v1/attempts",
			body:            `{"archive":"arm-reference","attempt":"aaaaaaaa","kind":"upload","tokenHash":"h","parts":["zim"]}`,
			wantAttemptCall: "register " + attA,
			check: func(t *testing.T, th *testHandler, _ []byte) {
				assert.Equal(t, libapi.RegisterRequest{Archive: armRef, Attempt: attA, Kind: libapi.KindUpload, TokenHash: "h", Parts: []string{"zim"}}, th.at.register)
			},
		},
		{name: "get attempt", method: http.MethodGet, path: "/v1/attempts/" + attA, wantAttemptCall: "get " + attA},
		{
			name: "promote", method: http.MethodPost, path: "/v1/attempts/" + attA + "/promote",
			body:          `{"expectedCurrentBookID":"` + uuidA + `","sourceVersion":"2026-11","sourceFingerprint":"url:u"}`,
			wantStoreCall: "promote " + attA,
			check: func(t *testing.T, th *testHandler, body []byte) {
				assert.Equal(t, libapi.PromoteRequest{ExpectedCurrentBookID: uuidA, SourceVersion: "2026-11", SourceFingerprint: "url:u"}, th.st.promote)
				assert.Contains(t, string(body), uuidB)
			},
		},
		{name: "discard", method: http.MethodPost, path: "/v1/attempts/" + attA + "/discard", wantAttemptCall: "discard " + attA},
		{
			name: "adopt", method: http.MethodPost, path: "/v1/archives/" + armRef + "/adopt",
			body:          `{"file":"x_2026-10.zim"}`,
			wantStoreCall: "adopt " + armRef,
			check: func(t *testing.T, th *testHandler, _ []byte) {
				assert.Equal(t, libapi.AdoptRequest{File: "x_2026-10.zim"}, th.st.adopt)
			},
		},
		{
			name: "library", method: http.MethodGet, path: "/v1/library",
			check: func(t *testing.T, _ *testHandler, body []byte) {
				var lib libapi.Library
				require.NoError(t, json.Unmarshal(body, &lib))
				assert.Equal(t, int64(7), lib.FreeBytes)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			th := newTestHandler(t)

			w := do(th, tc.method, tc.path, tc.body)

			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
			if tc.wantStoreCall != "" {
				assert.Equal(t, []string{tc.wantStoreCall}, th.st.calls)
			}
			if tc.wantAttemptCall != "" {
				assert.Equal(t, tc.wantAttemptCall, th.at.calls[0])
			}
			if tc.check != nil {
				tc.check(t, th, w.Body.Bytes())
			}
		})
	}
}

func TestMalformedBodyIsInvalid(t *testing.T) {
	th := newTestHandler(t)

	w := do(th, http.MethodPost, "/v1/attempts", `{"archive":`)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, libapi.CodeInvalid, errorBody(t, w).Code)
	assert.Empty(t, th.at.calls)
}

func TestUnknownRouteIsErrorBody(t *testing.T) {
	th := newTestHandler(t)

	for _, r := range []struct{ method, path string }{{http.MethodGet, "/v1/nothing"}, {http.MethodPost, "/v1/library"}} {
		w := do(th, r.method, r.path, "")

		assert.Equal(t, http.StatusNotFound, w.Code, r.path)
		assert.Equal(t, libapi.CodeNotFound, errorBody(t, w).Code, r.path)
	}
}

func TestStartupProbe(t *testing.T) {
	th := newTestHandler(t)
	th.started = false

	w := do(th, http.MethodGet, "/healthz/startup", "", "Authorization", "")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, libapi.CodeBusy, errorBody(t, w).Code)

	th.started = true
	w = do(th, http.MethodGet, "/healthz/startup", "", "Authorization", "")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestLivenessProbe(t *testing.T) {
	th := newTestHandler(t)
	th.started = false

	w := do(th, http.MethodGet, "/healthz/live", "", "Authorization", "")

	assert.Equal(t, http.StatusOK, w.Code)
}

// TestSnapshotReadsDoNotTakeLock holds S.mu, as a long confirmed write does, and shows the read endpoints still answer.
func TestSnapshotReadsDoNotTakeLock(t *testing.T) {
	a, ts := newTestAttempts(t)
	register(t, a, uploadReq(attB, "zim"))
	ts.stage(t, attA, armRef, "small-a.zim")
	ts.promote(t, attA, "")
	h := newLibrarianHandler(ts.Store, a, adminToken, func() bool { return true })

	ts.mu.Lock()
	defer ts.mu.Unlock()
	for _, path := range []string{"/v1/attempts/" + attB, "/v1/archives/" + armRef, "/v1/library"} {
		got := make(chan *httptest.ResponseRecorder, 1)
		go func() { got <- do(h, http.MethodGet, path, "") }()
		select {
		case w := <-got:
			assert.Equal(t, http.StatusOK, w.Code, path)
		case <-time.After(2 * time.Second):
			t.Errorf("GET %s blocked on the store lock", path)
		}
	}
}

func TestLibraryReportsCurrentFreeSpace(t *testing.T) {
	a, ts := newTestAttempts(t)
	h := newLibrarianHandler(ts.Store, a, adminToken, func() bool { return true })
	ts.Free = func(string) (int64, error) { return 42, nil }

	w := do(h, http.MethodGet, "/v1/library", "")

	require.Equal(t, http.StatusOK, w.Code)
	var lib libapi.Library
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &lib))
	assert.Equal(t, int64(42), lib.FreeBytes)
}

func TestWarcsStreamsTar(t *testing.T) {
	a, ts := newTestAttempts(t)
	srv := httptest.NewServer(newLibrarianHandler(ts.Store, a, adminToken, func() bool { return true }))
	t.Cleanup(srv.Close)
	tar := ts.warcsPath(armRef)
	require.NoError(t, os.MkdirAll(filepath.Dir(tar), 0o750))
	require.NoError(t, os.WriteFile(tar, []byte("tar bytes"), 0o600))

	get := func(archive string) (status int, contentType, body string) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/v1/archives/"+archive+"/warcs", http.NoBody)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := srv.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, resp.Header.Get("Content-Type"), string(data)
	}

	status, contentType, body := get(armRef)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "application/x-tar", contentType)
	assert.Equal(t, "tar bytes", body)

	status, _, body = get("other")
	assert.Equal(t, http.StatusNotFound, status)
	assert.Contains(t, body, libapi.CodeNotFound)
}

// TestPartPutBodyDeadline sends a part body over a real connection: a client that stops sending ends the body read within the idle bound, while a slow but steady client outlasts it.
func TestPartPutBodyDeadline(t *testing.T) {
	old := idleTimeout
	idleTimeout = 150 * time.Millisecond
	t.Cleanup(func() { idleTimeout = old })

	tests := []struct {
		name    string
		chunks  int
		wantErr bool
	}{
		{"stalled client times out", 1, true},
		{"steady client outlasts the bound", 8, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			th := newTestHandler(t)
			th.at.readBody, th.at.readDone = true, make(chan struct{})
			srv := httptest.NewServer(th)
			t.Cleanup(srv.Close)
			conn, err := net.Dial("tcp", srv.Listener.Addr().String())
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })

			const chunk = "0123456789"
			_, err = fmt.Fprintf(conn, "PUT /v1/attempts/%s/parts/zim HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nX-Content-SHA256: %s\r\nContent-Length: %d\r\n\r\n", attA, testToken, goodSHA, 8*len(chunk))
			require.NoError(t, err)
			for range tc.chunks {
				_, err = io.WriteString(conn, chunk)
				require.NoError(t, err)
				time.Sleep(60 * time.Millisecond)
			}

			select {
			case <-th.at.readDone:
			case <-time.After(5 * time.Second):
				t.Fatal("the body read never ended")
			}
			if tc.wantErr {
				require.ErrorIs(t, th.at.readErr, os.ErrDeadlineExceeded)
				return
			}
			require.NoError(t, th.at.readErr)
			assert.Equal(t, bytes.Repeat([]byte(chunk), 8), th.at.body)
		})
	}
}
