package librarian

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/jacaudi/kiwix-operator/internal/libapi"
)

// Fetcher downloads fetch attempts into the staged directory: Start runs one attempt's Run in its own goroutine, Cancel stops it.
type Fetcher struct {
	A *Attempts
	// HTTP sends the requests; nil means a client on http.DefaultTransport. A download has no total timeout: Attempts.IdleTimeout bounds silence instead.
	HTTP *http.Client
	// UserAgent is sent on every request, redirect hops included.
	UserAgent string
	// Sem bounds concurrent downloads, each holding one slot while it runs; nil means unlimited.
	Sem chan struct{}

	mu      sync.Mutex
	running map[string]*fetchRun
	stopped bool
}

// fetchRun is one Start goroutine. settling is set, under Fetcher.mu, once its download is over and it moves to its last state change, which can reach Cancel on this same goroutine through Store.OnTerminal.
type fetchRun struct {
	cancel   context.CancelFunc
	done     chan struct{}
	settling bool
}

// Start runs Run for attempt in a goroutine with its own context; a Start while the attempt's goroutine is still running, or after Stop, does nothing.
func (f *Fetcher) Start(attempt string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.running[attempt]; ok || f.stopped {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &fetchRun{cancel: cancel, done: make(chan struct{})}
	if f.running == nil {
		f.running = map[string]*fetchRun{}
	}
	f.running[attempt] = r
	go func() {
		defer f.exit(attempt, r)
		// Run records every outcome on the attempt itself; its error is for direct callers.
		_ = f.Run(ctx, attempt)
	}()
}

func (f *Fetcher) exit(attempt string, r *fetchRun) {
	r.cancel()
	f.mu.Lock()
	delete(f.running, attempt)
	f.mu.Unlock()
	close(r.done)
}

// Cancel stops attempt's download, waits for its goroutine to exit, then removes the partial file. It does not wait for a goroutine already settling, since that is where a Cancel reached through Store.OnTerminal comes from.
func (f *Fetcher) Cancel(attempt string) {
	f.mu.Lock()
	r := f.running[attempt]
	wait := r != nil && !r.settling
	f.mu.Unlock()
	if r != nil {
		r.cancel()
	}
	if wait {
		<-r.done
	}
	// A partial left behind is removed with the attempt's record by GC.
	_ = removeFile(f.A.S.partialPath(attempt, partZIM))
}

// Stop cancels every download and waits for its goroutine to exit, keeping the partial files so the next start resumes them with Range; Start does nothing afterwards.
func (f *Fetcher) Stop() {
	f.mu.Lock()
	f.stopped = true
	runs := slices.Collect(maps.Values(f.running))
	f.mu.Unlock()
	for _, r := range runs {
		r.cancel()
	}
	for _, r := range runs {
		<-r.done
	}
}

// Run downloads a registered or receiving fetch attempt into staged/<attempt>.zim.part, resuming the bytes already there, then verifies and completes it. A download error fails the attempt with its code; a cancelled ctx returns without touching the attempt, leaving the partial to resume or to Cancel.
func (f *Fetcher) Run(ctx context.Context, attempt string) error {
	if f.Sem != nil {
		select {
		case f.Sem <- struct{}{}:
			defer func() { <-f.Sem }()
		case <-ctx.Done():
			return fmt.Errorf("fetch %s: wait for a download slot: %w", attempt, ctx.Err())
		}
	}
	partial, spec, err := f.A.beginFetch(attempt)
	if err != nil {
		return err
	}
	sum, size, err := f.download(ctx, partial, spec)
	if ctx.Err() != nil {
		return fmt.Errorf("fetch %s: %w", attempt, errors.Join(ctx.Err(), err))
	}
	f.settle(attempt)
	if err != nil {
		err = fmt.Errorf("fetch %s: %w", attempt, err)
		return errors.Join(err, f.A.Fail(attempt, failCode(err), err.Error()))
	}
	return f.A.CompletePart(attempt, partZIM, partial, sum, size)
}

// settle marks attempt's Start goroutine, if it is the caller, as past its download.
func (f *Fetcher) settle(attempt string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.running[attempt]; r != nil {
		r.settling = true
	}
}

func failCode(err error) string {
	switch {
	case errors.Is(err, ErrInsufficientSpace):
		return libapi.CodeInsufficientSpace
	case errors.Is(err, ErrHashMismatch):
		return libapi.CodeHashMismatch
	}
	return libapi.CodeInternal
}

// download fetches spec.URL into partial, appending to the bytes already there when the server honors Range, and returns the file's hex SHA-256 and size. It holds no lock.
func (f *Fetcher) download(ctx context.Context, partial string, spec libapi.FetchSpec) (sum string, size int64, err error) {
	file, err := os.OpenFile(partial, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // partial is built from a registered attempt ID
	if err != nil {
		return "", 0, fmt.Errorf("open %s: %w", partial, err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	st, err := file.Stat()
	if err != nil {
		return "", 0, fmt.Errorf("stat %s: %w", partial, err)
	}
	resp, start, err := f.open(ctx, spec.URL, st.Size())
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only

	need := max(resp.ContentLength, 0)
	if spec.Size != 0 {
		need = spec.Size - start
	}
	if need < 0 {
		return "", 0, fmt.Errorf("declared size %d leaves %d bytes to fetch after the %d kept", spec.Size, need, start)
	}
	// Keeps need+1 and checkSpace's need+Reserve from overflowing on a hostile Content-Length.
	need = min(need, math.MaxInt64-1-f.A.Reserve)
	if err := f.A.checkSpace(need); err != nil {
		return "", 0, err
	}

	h := sha256.New()
	if err := file.Truncate(start); err != nil {
		return "", 0, fmt.Errorf("truncate %s: %w", partial, err)
	}
	if _, err := io.Copy(h, io.NewSectionReader(file, 0, start)); err != nil {
		return "", 0, fmt.Errorf("hash the kept bytes of %s: %w", partial, err)
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return "", 0, fmt.Errorf("seek %s: %w", partial, err)
	}
	var body io.Reader = resp.Body
	if spec.Size > 0 {
		// One byte past the declared size is enough to tell a longer body.
		body = io.LimitReader(resp.Body, need+1)
	}
	n, err := copyIdle(ctx, &spaceWriter{w: io.MultiWriter(file, h), a: f.A, left: need}, body, f.A.IdleTimeout)
	if err == nil {
		err = file.Sync()
	}
	if err != nil {
		return "", 0, fmt.Errorf("download into %s: %w", partial, err)
	}
	sum, size = hex.EncodeToString(h.Sum(nil)), start+n
	if err := checkReceived(spec, sum, size); err != nil {
		return "", 0, err
	}
	return sum, size, nil
}

// checkReceived compares what arrived with the size and SHA-256 the spec declares, when it declares them.
func checkReceived(spec libapi.FetchSpec, sum string, size int64) error {
	switch {
	case spec.Size > 0 && size > spec.Size:
		return fmt.Errorf("received more than declared size %d", spec.Size)
	case spec.Size > 0 && size < spec.Size:
		return fmt.Errorf("received %d of %d bytes", size, spec.Size)
	case spec.SHA256 != "" && !strings.EqualFold(sum, spec.SHA256):
		return fmt.Errorf("download hashes to %s, declared %s: %w", sum, spec.SHA256, ErrHashMismatch)
	}
	return nil
}

// open requests rawURL from byte have on and returns the response with the offset its body starts at: have for a 206 starting there, 0 for a 200. A 416, or a 206 starting elsewhere, restarts from zero once; any other status is an error naming only the host.
func (f *Fetcher) open(ctx context.Context, rawURL string, have int64) (*http.Response, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return nil, 0, redactURL(err)
	}
	// Go copies it to every redirect hop (net/http makeHeadersCopier); TestFetchFollowsRedirectAndStages checks the hop.
	req.Header.Set("User-Agent", f.UserAgent)
	if have > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(have, 10)+"-")
	}
	resp, err := cmp.Or(f.HTTP, &http.Client{}).Do(req)
	if err != nil {
		return nil, 0, redactURL(err)
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return resp, 0, nil
	case resp.StatusCode == http.StatusPartialContent && rangeStart(resp) == have:
		return resp, have, nil
	}
	_ = resp.Body.Close()
	if have > 0 && (resp.StatusCode == http.StatusRequestedRangeNotSatisfiable || resp.StatusCode == http.StatusPartialContent) {
		return f.open(ctx, rawURL, 0)
	}
	return nil, 0, fmt.Errorf("%s from %s", resp.Status, resp.Request.URL.Host)
}

// rangeStart returns the first byte offset of a 206's Content-Range, or -1 when it does not parse.
func rangeStart(resp *http.Response) int64 {
	spec, _ := strings.CutPrefix(resp.Header.Get("Content-Range"), "bytes ")
	first, _, _ := strings.Cut(spec, "-")
	n, err := strconv.ParseInt(first, 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// redactURL cuts the URL in a *url.Error in err down to its host, since a fetch URL's query may carry a token.
func redactURL(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		host := ""
		if u, perr := url.Parse(ue.URL); perr == nil {
			host = u.Host
		}
		ue.URL = host
	}
	return err
}

// beginFetch marks a registered or receiving fetch attempt receiving into its partial file and returns that path with the attempt's FetchSpec. The partial keeps its bytes for a resume.
func (a *Attempts) beginFetch(attempt string) (partial string, spec libapi.FetchSpec, err error) {
	s := a.S
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.M.Attempts[attempt]
	switch {
	case !ok:
		return "", spec, fmt.Errorf("attempt %s: %w", attempt, ErrNotFound)
	case rec.Kind != libapi.KindFetch || rec.Fetch == nil:
		return "", spec, fmt.Errorf("attempt %s is not a fetch: %w", attempt, ErrConflict)
	case rec.State != libapi.StateRegistered && rec.State != libapi.StateReceiving:
		return "", spec, fmt.Errorf("attempt %s is %s, not registered or receiving: %w", attempt, rec.State, ErrConflict)
	}
	partial = s.partialPath(attempt, partZIM)
	if err := os.MkdirAll(filepath.Dir(partial), 0o750); err != nil {
		return "", spec, fmt.Errorf("create %s: %w", filepath.Dir(partial), err)
	}
	rec.PartialPath = partial
	s.M.Attempts[attempt] = rec
	s.setState(attempt, libapi.StateReceiving, "", "")
	if err := s.save(); err != nil {
		return "", spec, err
	}
	return partial, *rec.Fetch, nil
}
