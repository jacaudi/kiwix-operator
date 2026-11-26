package librarian

import (
	"context"
	"crypto/subtle"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jacaudi/kiwix-operator/api/v1alpha1"
	"github.com/jacaudi/kiwix-operator/internal/libapi"
	"github.com/jacaudi/kiwix-operator/internal/zim"
)

const (
	// maxJSONBody caps every JSON request body; the largest, a registration, is well under 1 KiB.
	maxJSONBody = 1 << 20
	// busyRetryAfter is the Retry-After, in seconds, sent with 503 Busy.
	busyRetryAfter = "5"
)

var (
	// idleTimeout bounds silence on a part body and on a WARC stream: a receiving part returns to registered after 2 min with no bytes. A var so tests can shorten it.
	idleTimeout = 2 * time.Minute

	sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// StoreAPI is what the HTTP handler needs from the Store.
type StoreAPI interface {
	Current(archive string) *libapi.BookInfo
	Promote(ctx context.Context, attempt string, req libapi.PromoteRequest) (libapi.BookInfo, error)
	Adopt(ctx context.Context, archive string, req libapi.AdoptRequest) (libapi.BookInfo, error)
	Delete(ctx context.Context, archive, policy string) (done bool, err error)
}

// AttemptsAPI is what the HTTP handler needs from Attempts.
type AttemptsAPI interface {
	Register(req libapi.RegisterRequest) (libapi.AttemptState, error)
	PutPart(ctx context.Context, attempt, part, token, declaredSHA string, length int64, body io.Reader) (libapi.AttemptState, error)
	Get(attempt string) (libapi.AttemptState, error)
	Discard(attempt string) error
}

// statusOf maps each sentinel to its status and ErrorBody code. The first match wins, so a sentinel that can wrap with another comes first.
var statusOf = []struct {
	err    error
	status int
	code   string
}{
	{ErrInvalid, http.StatusBadRequest, libapi.CodeInvalid},
	{ErrAdoptInvalid, http.StatusBadRequest, libapi.CodeAdoptFileInvalid},
	{ErrUnauthorized, http.StatusUnauthorized, libapi.CodeUnauthorized},
	{ErrNotFound, http.StatusNotFound, libapi.CodeNotFound},
	{ErrAdoptMissing, http.StatusNotFound, libapi.CodeAdoptFileMissing},
	{ErrCurrentChanged, http.StatusConflict, libapi.CodeCurrentChanged},
	{ErrDuplicateBook, http.StatusConflict, libapi.CodeDuplicateBook},
	{ErrConflict, http.StatusConflict, libapi.CodeConflict},
	{ErrHashMismatch, http.StatusUnprocessableEntity, libapi.CodeHashMismatch},
	{ErrInvalidZim, http.StatusUnprocessableEntity, libapi.CodeInvalidZim},
	{ErrBusy, http.StatusServiceUnavailable, libapi.CodeBusy},
	{ErrInsufficientSpace, http.StatusInsufficientStorage, libapi.CodeInsufficientSpace},
}

type server struct {
	st         StoreAPI
	at         AttemptsAPI
	adminToken []byte
	lib        func() libapi.Library
	warcs      func(archive string) (io.ReadCloser, error)
	started    func() bool
}

// NewHandler serves the librarian API. Every /v1 route takes the admin token except the part upload, which takes the attempt's own token; the health probes take none. Paths whose segments are not all DNS labels are refused before routing.
func NewHandler(st StoreAPI, at AttemptsAPI, adminToken string, lib func() libapi.Library, warcs func(archive string) (io.ReadCloser, error), started func() bool) http.Handler {
	s := &server{st: st, at: at, adminToken: []byte(adminToken), lib: lib, warcs: warcs, started: started}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/attempts", s.admin(s.register))
	mux.HandleFunc("GET /v1/attempts/{attempt}", s.admin(s.getAttempt))
	mux.HandleFunc("POST /v1/attempts/{attempt}/promote", s.admin(s.promote))
	mux.HandleFunc("POST /v1/attempts/{attempt}/discard", s.admin(s.discard))
	mux.HandleFunc("PUT /v1/attempts/{attempt}/parts/{part}", s.putPart)
	mux.HandleFunc("GET /v1/archives/{archive}", s.admin(s.getArchive))
	mux.HandleFunc("POST /v1/archives/{archive}/adopt", s.admin(s.adopt))
	mux.HandleFunc("DELETE /v1/archives/{archive}", s.admin(s.deleteArchive))
	mux.HandleFunc("GET /v1/archives/{archive}/warcs", s.admin(s.getWarcs))
	mux.HandleFunc("GET /v1/library", s.admin(s.library))
	mux.HandleFunc("GET /healthz/startup", s.startup)
	mux.HandleFunc("GET /healthz/live", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	// Matches whatever no route does, wrong methods included, so even those answer with an ErrorBody.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, libapi.CodeNotFound, "no such route")
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validPath(r.URL.EscapedPath()) {
			writeError(w, http.StatusBadRequest, libapi.CodeInvalid, "every path segment must be a lowercase DNS label")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// validPath reports whether every segment of the escaped path p after its leading slash is a DNS label. That refuses ".", "..", empty segments and every escape, %2F included, before the mux could clean or redirect.
func validPath(p string) bool {
	rest, ok := strings.CutPrefix(p, "/")
	if !ok {
		return false
	}
	for seg := range strings.SplitSeq(rest, "/") {
		// Archive and attempt segments must be DNS labels, and so is every literal segment of every route.
		if !zim.ValidArchiveName(seg) {
			return false
		}
	}
	return true
}

func (s *server) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearer(r)
		if !ok || subtle.ConstantTimeCompare([]byte(token), s.adminToken) != 1 {
			writeError(w, http.StatusUnauthorized, libapi.CodeUnauthorized, "this route needs the admin token")
			return
		}
		h(w, r)
	}
}

func bearer(r *http.Request) (string, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return token, ok && token != ""
}

func (s *server) register(w http.ResponseWriter, r *http.Request) {
	var req libapi.RegisterRequest
	if !decode(w, r, &req) {
		return
	}
	st, err := s.at.Register(req)
	reply(w, st, err)
}

func (s *server) getAttempt(w http.ResponseWriter, r *http.Request) {
	st, err := s.at.Get(r.PathValue("attempt"))
	reply(w, st, err)
}

func (s *server) promote(w http.ResponseWriter, r *http.Request) {
	var req libapi.PromoteRequest
	if !decode(w, r, &req) {
		return
	}
	info, err := s.st.Promote(r.Context(), r.PathValue("attempt"), req)
	reply(w, info, err)
}

func (s *server) discard(w http.ResponseWriter, r *http.Request) {
	attempt := r.PathValue("attempt")
	if err := s.at.Discard(attempt); err != nil {
		fail(w, err)
		return
	}
	st, err := s.at.Get(attempt)
	reply(w, st, err)
}

// putPart streams one part to Attempts.PutPart, which checks the upload token against the attempt.
func (s *server) putPart(w http.ResponseWriter, r *http.Request) {
	token, ok := bearer(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, libapi.CodeUnauthorized, "a part upload needs the attempt's upload token")
		return
	}
	part, sha := r.PathValue("part"), r.Header.Get("X-Content-SHA256")
	switch {
	case part != partZIM && part != partWARCs:
		writeError(w, http.StatusBadRequest, libapi.CodeInvalid, "part must be "+partZIM+" or "+partWARCs)
		return
	case !sha256RE.MatchString(sha):
		writeError(w, http.StatusBadRequest, libapi.CodeInvalid, "X-Content-SHA256 must be 64 lowercase hex characters")
		return
	case r.ContentLength < 0:
		writeError(w, http.StatusBadRequest, libapi.CodeInvalid, "Content-Length is required")
		return
	}
	rc := http.NewResponseController(w)
	// The reply is a few hundred bytes sent once the whole body is in, long after WriteTimeout's deadline for a large part; unsupported only off a real connection.
	_ = rc.SetWriteDeadline(time.Time{})
	// Re-armed on every read instead of ReadTimeout, which would cut any upload longer than it: a client that stops sending fails the read within idleTimeout, so even a read PutPart abandoned returns and its goroutine exits.
	body := &idleReader{r: r.Body, rc: rc}
	st, err := s.at.PutPart(r.Context(), r.PathValue("attempt"), part, token, sha, r.ContentLength, body)
	reply(w, st, err)
}

func (s *server) getArchive(w http.ResponseWriter, r *http.Request) {
	archive := r.PathValue("archive")
	info := s.st.Current(archive)
	if info == nil {
		writeError(w, http.StatusNotFound, libapi.CodeNotFound, "archive "+archive+" has no current book")
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *server) adopt(w http.ResponseWriter, r *http.Request) {
	var req libapi.AdoptRequest
	if !decode(w, r, &req) {
		return
	}
	info, err := s.st.Adopt(r.Context(), r.PathValue("archive"), req)
	reply(w, info, err)
}

// deleteArchive answers 200 once the archive is gone and 202 while its file waits for kiwix-serve to drop the book.
func (s *server) deleteArchive(w http.ResponseWriter, r *http.Request) {
	policy := r.URL.Query().Get("policy")
	if policy != string(v1alpha1.DeletionPolicyRetain) && policy != string(v1alpha1.DeletionPolicyDelete) {
		writeError(w, http.StatusBadRequest, libapi.CodeInvalid, "policy must be Retain or Delete")
		return
	}
	done, err := s.st.Delete(r.Context(), r.PathValue("archive"), policy)
	switch {
	case err != nil:
		fail(w, err)
	case done:
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

func (s *server) getWarcs(w http.ResponseWriter, r *http.Request) {
	tar, err := s.warcs(r.PathValue("archive"))
	if err != nil {
		fail(w, err)
		return
	}
	defer tar.Close() //nolint:errcheck // read-only
	w.Header().Set("Content-Type", "application/x-tar")
	// The status is sent; a failed copy means the reader went away or stalled past idleTimeout, and there is no one left to tell.
	_, _ = io.Copy(&idleWriter{w: w, rc: http.NewResponseController(w)}, tar)
}

func (s *server) library(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.lib())
}

func (s *server) startup(w http.ResponseWriter, _ *http.Request) {
	if !s.started() {
		writeError(w, http.StatusServiceUnavailable, libapi.CodeBusy, "the librarian is starting")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// idleReader re-arms the connection's read deadline before every read.
type idleReader struct {
	r  io.Reader
	rc *http.ResponseController
}

func (b *idleReader) Read(p []byte) (int, error) {
	if err := b.rc.SetReadDeadline(time.Now().Add(idleTimeout)); err != nil {
		return 0, err
	}
	return b.r.Read(p)
}

// idleWriter re-arms the connection's write deadline before every write, so a long stream outlasts WriteTimeout while a stalled reader does not.
type idleWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (b *idleWriter) Write(p []byte) (int, error) {
	if err := b.rc.SetWriteDeadline(time.Now().Add(idleTimeout)); err != nil {
		return 0, err
	}
	return b.w.Write(p)
}

// decode reads a JSON request body into v, answering 400 Invalid and returning false when it does not parse.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.UnmarshalRead(http.MaxBytesReader(w, r.Body, maxJSONBody), v); err != nil {
		writeError(w, http.StatusBadRequest, libapi.CodeInvalid, "decode request body: "+err.Error())
		return false
	}
	return true
}

func reply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// fail answers with err's mapped status and code; the message is err's text, since the admin and upload tokens' holders are the operator's own components.
func fail(w http.ResponseWriter, err error) {
	for _, m := range statusOf {
		if errors.Is(err, m.err) {
			if m.code == libapi.CodeBusy {
				w.Header().Set("Retry-After", busyRetryAfter)
			}
			writeError(w, m.status, m.code, err.Error())
			return
		}
	}
	writeError(w, http.StatusInternalServerError, libapi.CodeInternal, err.Error())
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, libapi.ErrorBody{Code: code, Message: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status is sent; a failed body write means the client went away, and there is no one left to tell.
	_ = json.MarshalWrite(w, v)
}
