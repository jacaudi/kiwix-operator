// Package libclient is the typed client of the librarian sidecar's HTTP API, used by the manager's controllers and the crawl pod's uploader.
package libclient

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jacaudi/kiwix-operator/internal/libapi"
)

const (
	// maxResponse caps every response body read; librarian replies are a few KiB, a library summary the largest.
	maxResponse = 4 << 20
	// maxErrorText caps how much of an unparseable error body goes into the error.
	maxErrorText = 512
)

// The per-call bounds are variables only so a test can shorten them. They are applied with a context, so an injected HTTP client cannot remove them.
var (
	// callTimeout bounds every call except the three below.
	callTimeout = 10 * time.Second
	// longCallTimeout bounds Promote, Adopt and Delete, which wait on up to three confirmation windows.
	longCallTimeout = 120 * time.Second
)

// Sentinels for the librarian's error codes, matched with errors.Is. Other codes, such as InvalidZim, come back as an error carrying the code and the server's message.
var (
	ErrCurrentChanged    = errors.New("current book changed")
	ErrDuplicateBook     = errors.New("duplicate book")
	ErrConflict          = errors.New("conflict")
	ErrUnauthorized      = errors.New("unauthorized")
	ErrNotFound          = errors.New("not found")
	ErrInsufficientSpace = errors.New("insufficient space")
	ErrHashMismatch      = errors.New("hash mismatch")
	ErrAdoptMissing      = errors.New("adoptFile missing")
	ErrAdoptInvalid      = errors.New("adoptFile invalid")
	// ErrBusy means the librarian is starting or occupied; the caller may retry.
	ErrBusy = errors.New("librarian busy")
)

var sentinelOf = map[string]error{
	libapi.CodeCurrentChanged:    ErrCurrentChanged,
	libapi.CodeDuplicateBook:     ErrDuplicateBook,
	libapi.CodeConflict:          ErrConflict,
	libapi.CodeUnauthorized:      ErrUnauthorized,
	libapi.CodeNotFound:          ErrNotFound,
	libapi.CodeInsufficientSpace: ErrInsufficientSpace,
	libapi.CodeHashMismatch:      ErrHashMismatch,
	libapi.CodeAdoptFileMissing:  ErrAdoptMissing,
	libapi.CodeAdoptFileInvalid:  ErrAdoptInvalid,
	libapi.CodeBusy:              ErrBusy,
}

// apiError is a non-2xx librarian reply. code is empty when the body was not an ErrorBody.
type apiError struct {
	status int
	code   string
	msg    string
}

func (e *apiError) Error() string {
	if e.code == "" {
		return fmt.Sprintf("librarian: HTTP %d: %s", e.status, e.msg)
	}
	return fmt.Sprintf("librarian: %s (HTTP %d): %s", e.code, e.status, e.msg)
}

// Unwrap returns the sentinel for e's code, or nil for a code without one.
func (e *apiError) Unwrap() error { return sentinelOf[e.code] }

// Client calls the librarian API. Admin calls carry Token; PutPart carries the attempt's own upload token.
type Client struct {
	// BaseURL is the librarian root, such as http://kiwix-librarian:8090.
	BaseURL string
	// Token is the admin token.
	Token string
	// HTTP defaults to http.DefaultClient; each call is bounded by its own context either way.
	HTTP *http.Client
}

// Register registers an attempt.
func (c *Client) Register(ctx context.Context, req libapi.RegisterRequest) (libapi.AttemptState, error) {
	var out libapi.AttemptState
	_, err := c.json(ctx, callTimeout, http.MethodPost, "/v1/attempts", req, &out)
	return out, err
}

// Get reports an attempt's state.
func (c *Client) Get(ctx context.Context, attempt string) (libapi.AttemptState, error) {
	var out libapi.AttemptState
	_, err := c.json(ctx, callTimeout, http.MethodGet, "/v1/attempts/"+url.PathEscape(attempt), nil, &out)
	return out, err
}

// Promote makes a staged book current.
func (c *Client) Promote(ctx context.Context, attempt string, req libapi.PromoteRequest) (libapi.BookInfo, error) {
	var out libapi.BookInfo
	_, err := c.json(ctx, longCallTimeout, http.MethodPost, "/v1/attempts/"+url.PathEscape(attempt)+"/promote", req, &out)
	return out, err
}

// Discard abandons an attempt and reports its final state.
func (c *Client) Discard(ctx context.Context, attempt string) (libapi.AttemptState, error) {
	var out libapi.AttemptState
	_, err := c.json(ctx, callTimeout, http.MethodPost, "/v1/attempts/"+url.PathEscape(attempt)+"/discard", nil, &out)
	return out, err
}

// Archive returns an archive's current book, or nil, nil when it has none.
func (c *Client) Archive(ctx context.Context, archive string) (*libapi.BookInfo, error) {
	var out libapi.BookInfo
	_, err := c.json(ctx, callTimeout, http.MethodGet, "/v1/archives/"+url.PathEscape(archive), nil, &out)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Adopt makes an existing unmanaged file an archive's current book.
func (c *Client) Adopt(ctx context.Context, archive string, req libapi.AdoptRequest) (libapi.BookInfo, error) {
	var out libapi.BookInfo
	_, err := c.json(ctx, longCallTimeout, http.MethodPost, "/v1/archives/"+url.PathEscape(archive)+"/adopt", req, &out)
	return out, err
}

// Delete removes an archive under policy, Retain or Delete. done is false while the librarian answers 202, its file still waiting for kiwix-serve to drop the book; call again.
func (c *Client) Delete(ctx context.Context, archive, policy string) (done bool, err error) {
	target := "/v1/archives/" + url.PathEscape(archive) + "?" + url.Values{"policy": {policy}}.Encode()
	status, err := c.json(ctx, longCallTimeout, http.MethodDelete, target, nil, nil)
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusOK:
		return true, nil
	case http.StatusAccepted:
		return false, nil
	default:
		return false, &apiError{status: status, msg: "unexpected success status for delete"}
	}
}

// Library summarizes what the librarian serves.
func (c *Client) Library(ctx context.Context) (libapi.Library, error) {
	var out libapi.Library
	_, err := c.json(ctx, callTimeout, http.MethodGet, "/v1/library", nil, &out)
	return out, err
}

// PutPart streams length bytes of body as one part of an upload attempt, authorized by that attempt's upload token and checked against the SHA-256 sha. The call has no time bound of its own, only ctx: a part may take hours.
func (c *Client) PutPart(ctx context.Context, attempt, part, token, sha string, length int64, body io.Reader) error {
	if length < 0 {
		return fmt.Errorf("put part %s: negative length %d", part, length)
	}
	// A zero-length body that is not NoBody goes out chunked, which the librarian refuses.
	if length == 0 {
		body = http.NoBody
	}
	target := "/v1/attempts/" + url.PathEscape(attempt) + "/parts/" + url.PathEscape(part)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.BaseURL+target, body)
	if err != nil {
		return fmt.Errorf("build put part request: %w", err)
	}
	// The librarian refuses a chunked body or a missing Content-Length, and net/http cannot size an arbitrary reader itself.
	req.ContentLength = length
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Content-SHA256", sha)
	_, err = c.send(req, nil)
	return err
}

// json sends an admin call whose request and response bodies are JSON, bounded by timeout, and returns the success status.
func (c *Client) json(ctx context.Context, timeout time.Duration, method, target string, in, out any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, fmt.Errorf("encode %s %s request: %w", method, target, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+target, body)
	if err != nil {
		return 0, fmt.Errorf("build %s %s request: %w", method, target, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.send(req, out)
}

// send performs req, turns a non-2xx reply into an *apiError and decodes a 2xx body into out when out is not nil. It returns the status.
func (c *Client) send(req *http.Request, out any) (int, error) {
	resp, err := cmp.Or(c.HTTP, http.DefaultClient).Do(req)
	if err != nil {
		return 0, fmt.Errorf("librarian request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read librarian response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, errorFrom(resp.StatusCode, data)
	}
	if len(data) > maxResponse {
		return resp.StatusCode, fmt.Errorf("librarian response exceeds %d bytes", maxResponse)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode librarian response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// errorFrom builds the error for a non-2xx reply: the ErrorBody when body is one, else the status and the first maxErrorText bytes of body.
func errorFrom(status int, body []byte) error {
	var eb libapi.ErrorBody
	if err := json.Unmarshal(body, &eb); err == nil && eb.Code != "" {
		return &apiError{status: status, code: eb.Code, msg: eb.Message}
	}
	return &apiError{status: status, msg: strconv.Quote(string(body[:min(len(body), maxErrorText)]))}
}
