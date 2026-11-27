package librarian

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jacaudi/kiwix-operator/internal/libapi"
	"github.com/jacaudi/kiwix-operator/internal/zim"
)

const (
	// terminalRetention is how long GC keeps the record of a promoted, discarded or failed attempt.
	terminalRetention = 7 * 24 * time.Hour
	partialExt        = ".part"
)

var (
	// ErrBusy means another body for the attempt is still being received; it is transient, so the server answers 503 with Retry-After and the uploader retries.
	ErrBusy = errors.New("busy")
	// ErrInvalid means a registration is malformed.
	ErrInvalid = errors.New("invalid")
	// ErrUnauthorized means the upload token does not match the attempt's token hash.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrInsufficientSpace means writing the part would eat into the reserve.
	ErrInsufficientSpace = errors.New("insufficient space")
	// ErrHashMismatch means a part's body does not hash to its declared SHA-256.
	ErrHashMismatch = errors.New("hash mismatch")
	// ErrInvalidZim means the attempt's ZIM could not be read; the attempt has failed.
	ErrInvalidZim = errors.New("invalid zim")

	attemptIDRE = regexp.MustCompile(`^[a-z2-7]{8}$`)
)

// Attempts runs the attempt state machine over S. It has no lock of its own: every manifest access holds S.mu, every state change goes through S.setState, and S.OnTerminal is called after S.mu is released.
type Attempts struct {
	S *Store
	// Reserve is the free space, in bytes, a part may never eat into.
	Reserve int64
	// Now is the clock for deadlines and garbage collection; nil means time.Now.
	Now func() time.Time
	// IdleTimeout aborts a part body that delivers no bytes for this long; it must be positive.
	IdleTimeout time.Duration
	// OnFetchRegistered, when set, is called with each newly registered fetch attempt, after S.mu is released.
	OnFetchRegistered func(attempt string)
}

// Register records a new attempt in state registered. A repeat with the same archive and token hash returns the attempt's state; anything else for a known attempt is ErrConflict.
func (a *Attempts) Register(req libapi.RegisterRequest) (libapi.AttemptState, error) {
	if err := validateRegister(req); err != nil {
		return libapi.AttemptState{}, err
	}
	if req.Kind == libapi.KindFetch {
		req.Parts = []string{partZIM}
	}
	st, created, err := a.register(req)
	if created && req.Kind == libapi.KindFetch && a.OnFetchRegistered != nil {
		a.OnFetchRegistered(req.Attempt)
	}
	return st, err
}

func (a *Attempts) register(req libapi.RegisterRequest) (st libapi.AttemptState, created bool, err error) {
	s := a.S
	s.mu.Lock()
	defer s.mu.Unlock()

	if rec, ok := s.M.Attempts[req.Attempt]; ok {
		if rec.Archive != req.Archive || rec.TokenHash != req.TokenHash {
			return st, false, fmt.Errorf("attempt %s is already registered for archive %s with another token: %w", req.Attempt, rec.Archive, ErrConflict)
		}
		return s.attempt(req.Attempt), false, nil
	}
	s.M.Attempts[req.Attempt] = AttemptRecord{RegisterRequest: req, Parts: map[string]PartRecord{}}
	s.setState(req.Attempt, libapi.StateRegistered, "", "")
	if err := s.save(); err != nil {
		return st, false, err
	}
	return s.attempt(req.Attempt), true, nil
}

// validateRegister checks every field that later becomes part of a file name or decides the attempt's flow, reporting all problems together.
func validateRegister(req libapi.RegisterRequest) error {
	var errs []error
	if !attemptIDRE.MatchString(req.Attempt) {
		errs = append(errs, fmt.Errorf("attempt %q must be 8 characters from [a-z2-7]", req.Attempt))
	}
	if !zim.ValidArchiveName(req.Archive) {
		errs = append(errs, fmt.Errorf("archive %q must be a DNS label of at most 45 characters", req.Archive))
	}
	switch req.Kind {
	case libapi.KindUpload:
		if !slices.Contains(req.Parts, partZIM) {
			errs = append(errs, fmt.Errorf("an upload needs the %s part", partZIM))
		}
		for _, p := range req.Parts {
			if p != partZIM && p != partWARCs {
				errs = append(errs, fmt.Errorf("part %q: want %s or %s", p, partZIM, partWARCs))
			}
		}
	case libapi.KindFetch:
		if req.Fetch == nil || req.Fetch.URL == "" {
			errs = append(errs, errors.New("a fetch needs fetch.url"))
		}
	default:
		errs = append(errs, fmt.Errorf("kind %q: want %s or %s", req.Kind, libapi.KindUpload, libapi.KindFetch))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("register: %w: %w", ErrInvalid, err)
	}
	return nil
}

// PutPart receives one part of an upload attempt. The body is streamed to staged/<attempt>.<part>.part with no lock held, verified against declaredSHA and renamed into place; the attempt becomes staged once every part is in and its ZIM reads. A part already received answers with the attempt's state when declaredSHA matches.
func (a *Attempts) PutPart(ctx context.Context, attempt, part, token, declaredSHA string, length int64, body io.Reader) (libapi.AttemptState, error) {
	partial, st, err := a.beginPart(attempt, part, token, declaredSHA, length)
	if err != nil || partial == "" {
		return st, err
	}
	sum, n, err := a.receive(ctx, partial, length, body)
	if err == nil && sum != declaredSHA {
		err = fmt.Errorf("part %s of %s hashes to %s, declared %s: %w", part, attempt, sum, declaredSHA, ErrHashMismatch)
	}
	return a.endPart(ctx, attempt, part, partial, declaredSHA, n, err)
}

// beginPart checks the request under S.mu and marks the attempt receiving into its partial file. An empty partial with a nil error means the part was already received with this hash.
func (a *Attempts) beginPart(attempt, part, token, declaredSHA string, length int64) (partial string, st libapi.AttemptState, err error) {
	s := a.S
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.M.Attempts[attempt]
	switch {
	case !ok:
		return "", st, fmt.Errorf("attempt %s: %w", attempt, ErrNotFound)
	case !tokenMatches(token, rec.TokenHash):
		return "", st, fmt.Errorf("attempt %s: %w", attempt, ErrUnauthorized)
	case rec.Kind != libapi.KindUpload || !slices.Contains(rec.RegisterRequest.Parts, part):
		return "", st, fmt.Errorf("attempt %s takes no %s upload: %w", attempt, part, ErrConflict)
	}
	if got, ok := rec.Parts[part]; ok && got.Done {
		if got.SHA256 != declaredSHA {
			return "", st, fmt.Errorf("part %s of %s was already received with hash %s: %w", part, attempt, got.SHA256, ErrConflict)
		}
		return "", s.attempt(attempt), nil
	}
	switch rec.State {
	case libapi.StateRegistered:
	case libapi.StateReceiving:
		// Clears once the earlier body ends, by completion, disconnect or IdleTimeout.
		return "", st, fmt.Errorf("attempt %s is still receiving another body: %w", attempt, ErrBusy)
	default:
		return "", st, fmt.Errorf("attempt %s is %s, not registered: %w", attempt, rec.State, ErrConflict)
	}
	if err := a.checkSpace(length); err != nil {
		err = fmt.Errorf("part %s of %s: %w", part, attempt, err)
		code, msg := refusal(err)
		if code == "" {
			return "", st, err
		}
		// The attempt stays registered, so the code is what tells the manager why the upload never staged.
		s.setState(attempt, libapi.StateRegistered, code, msg)
		return "", st, errors.Join(err, s.save())
	}
	partial = s.partialPath(attempt, part)
	if err := os.MkdirAll(filepath.Dir(partial), 0o750); err != nil {
		return "", st, fmt.Errorf("create %s: %w", filepath.Dir(partial), err)
	}
	rec.PartialPath = partial
	s.M.Attempts[attempt] = rec
	s.setState(attempt, libapi.StateReceiving, "", "")
	if err := s.save(); err != nil {
		return "", st, err
	}
	return partial, st, nil
}

// receive streams at most length bytes of body into partial and returns their hex SHA-256 and count. It runs without S.mu and aborts when no bytes arrive for IdleTimeout, when ctx ends, or when free space falls below what is left plus Reserve.
func (a *Attempts) receive(ctx context.Context, partial string, length int64, body io.Reader) (sum string, n int64, err error) {
	f, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // partial is built from a registered attempt ID and part name
	if err != nil {
		return "", 0, fmt.Errorf("create %s: %w", partial, err)
	}
	h := sha256.New()
	n, err = copyIdle(ctx, &spaceWriter{w: io.MultiWriter(f, h), a: a, left: length}, io.LimitReader(body, length), a.IdleTimeout)
	if err == nil {
		err = f.Sync()
	}
	if err := errors.Join(err, f.Close()); err != nil {
		return "", n, fmt.Errorf("receive %s: %w", partial, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// endPart settles a streamed body under S.mu. An attempt ended meanwhile only loses the partial; a failed body sends the attempt back to registered; a good one is moved into place and recorded.
func (a *Attempts) endPart(ctx context.Context, attempt, part, partial, sha string, size int64, err error) (st libapi.AttemptState, _ error) {
	s := a.S
	var terminal []string
	defer func() { s.notify(terminal) }()
	s.mu.Lock()
	defer s.mu.Unlock()

	if rec, ok := s.M.Attempts[attempt]; !ok || rec.State != libapi.StateReceiving || rec.PartialPath != partial {
		// Whoever ended it removed the staged files, but the partial may have been created after that.
		return st, errors.Join(fmt.Errorf("attempt %s ended while part %s was being received: %w", attempt, part, ErrConflict), removeFile(partial))
	}
	if err == nil {
		err = s.placePart(attempt, part, partial)
	}
	if err != nil {
		rec := s.M.Attempts[attempt]
		rec.PartialPath = ""
		s.M.Attempts[attempt] = rec
		code, msg := refusal(err)
		s.setState(attempt, libapi.StateRegistered, code, msg)
		return st, errors.Join(err, removeFile(partial), s.save())
	}
	// The body is in; a client hanging up now must not fail the ZIM read.
	if terminal, err = a.partDone(context.WithoutCancel(ctx), attempt, part, sha, size); err != nil {
		return st, err
	}
	return s.attempt(attempt), nil
}

// CompletePart records a part the fetcher downloaded and verified at path: the file moves into place and the attempt becomes staged once every part is in and its ZIM reads. The attempt must be receiving.
func (a *Attempts) CompletePart(attempt, part, path, sha string, size int64) error {
	s := a.S
	var terminal []string
	defer func() { s.notify(terminal) }()
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.M.Attempts[attempt]
	switch {
	case !ok:
		return fmt.Errorf("attempt %s: %w", attempt, ErrNotFound)
	case rec.State != libapi.StateReceiving:
		return fmt.Errorf("attempt %s is %s, not receiving: %w", attempt, rec.State, ErrConflict)
	}
	if err := s.placePart(attempt, part, path); err != nil {
		return err
	}
	var err error
	terminal, err = a.partDone(context.Background(), attempt, part, sha, size)
	return err
}

// partDone records a part that is in place. Until every part is, the attempt waits in registered; then its ZIM is read and the attempt becomes staged, or failed with InvalidZim and its staged files removed. The caller holds S.mu.
func (a *Attempts) partDone(ctx context.Context, attempt, part, sha string, size int64) ([]string, error) {
	s := a.S
	rec := s.M.Attempts[attempt]
	if rec.Parts == nil {
		rec.Parts = map[string]PartRecord{}
	}
	rec.Parts[part] = PartRecord{SHA256: sha, Size: size, Done: true}
	rec.PartialPath = ""
	s.M.Attempts[attempt] = rec
	if slices.ContainsFunc(rec.RegisterRequest.Parts, func(p string) bool { return !rec.Parts[p].Done }) {
		s.setState(attempt, libapi.StateRegistered, "", "")
		return nil, s.save()
	}

	path := s.stagedPath(attempt, "."+partZIM)
	id, err := zim.ReadUUID(path)
	var book Book
	if err == nil {
		book, err = readBook(ctx, s.R, path, id)
	}
	if err != nil {
		terminal := s.setState(attempt, libapi.StateFailed, libapi.CodeInvalidZim, err.Error())
		if saveErr := s.save(); saveErr != nil {
			return nil, saveErr
		}
		return terminal, errors.Join(fmt.Errorf("attempt %s: %w: %w", attempt, ErrInvalidZim, err), s.removeStaged(attempt))
	}
	rec.Staged = &book
	s.M.Attempts[attempt] = rec
	s.setState(attempt, libapi.StateStaged, "", "")
	return nil, s.save()
}

// refusal is the code and message a registered attempt carries after a body was refused for space; any other error leaves neither.
func refusal(err error) (code, msg string) {
	if errors.Is(err, ErrInsufficientSpace) {
		return libapi.CodeInsufficientSpace, err.Error()
	}
	return "", ""
}

// placePart renames a received part to staged/<attempt>.<part> and syncs the directory.
func (s *Store) placePart(attempt, part, src string) error {
	dst := s.stagedPath(attempt, "."+part)
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("move %s to %s: %w", src, dst, err)
	}
	return syncDir(filepath.Dir(dst))
}

// partialPath is where a part is written while it arrives: staged/<attempt>.<part>.part.
func (s *Store) partialPath(attempt, part string) string {
	return s.stagedPath(attempt, "."+part+partialExt)
}

// ResetReceiving sends every upload attempt that was receiving when the librarian stopped back to registered and deletes its partial file; fetch attempts keep theirs to resume. Call it at startup, before serving.
func (a *Attempts) ResetReceiving() error {
	s := a.S
	s.mu.Lock()
	defer s.mu.Unlock()

	var errs []error
	for id, rec := range s.M.Attempts {
		if rec.Kind != libapi.KindUpload || rec.State != libapi.StateReceiving {
			continue
		}
		errs = append(errs, removeFile(rec.PartialPath))
		rec.PartialPath = ""
		s.M.Attempts[id] = rec
		s.setState(id, libapi.StateRegistered, "", "")
	}
	return errors.Join(append(errs, s.save())...)
}

// Discard ends an open attempt as discarded and removes its staged files. A terminal attempt is left as it is; a promoting one is ErrConflict.
func (a *Attempts) Discard(attempt string) error {
	return a.end(attempt, libapi.StateDiscarded, "", "")
}

// Fail ends an open attempt as failed with code and msg and removes its staged files. A terminal attempt is left as it is; a promoting one is ErrConflict.
func (a *Attempts) Fail(attempt, code, msg string) error {
	return a.end(attempt, libapi.StateFailed, code, msg)
}

func (a *Attempts) end(attempt, state, code, msg string) error {
	s := a.S
	var terminal []string
	defer func() { s.notify(terminal) }()
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.M.Attempts[attempt]
	switch {
	case !ok:
		return fmt.Errorf("attempt %s: %w", attempt, ErrNotFound)
	case terminalState(rec.State):
		return nil
	case rec.State == libapi.StatePromoting:
		return fmt.Errorf("attempt %s is promoting: %w", attempt, ErrConflict)
	}
	t := s.setState(attempt, state, code, msg)
	// Saved first, so a failed save leaves the attempt open with its files.
	if err := s.save(); err != nil {
		return err
	}
	terminal = t
	return s.removeStaged(attempt)
}

// Get returns the attempt's state from the snapshot, without taking S.mu.
func (a *Attempts) Get(attempt string) (libapi.AttemptState, error) {
	st, ok := a.S.Snapshot().Attempts[attempt]
	if !ok {
		return st, fmt.Errorf("attempt %s: %w", attempt, ErrNotFound)
	}
	return st, nil
}

// GC discards open attempts past their deadline, forgets attempts terminal for longer than terminalRetention, and removes the staged files of attempts it discarded or that are not registered. It returns how many files it removed.
func (a *Attempts) GC() (removed int, err error) {
	s := a.S
	var terminal []string
	defer func() { s.notify(terminal) }()
	s.mu.Lock()
	defer s.mu.Unlock()

	now := a.now()
	expired := map[string]bool{}
	var t []string
	for id, rec := range s.M.Attempts {
		switch {
		case terminalState(rec.State):
			if now.Sub(rec.UpdatedAt) > terminalRetention {
				delete(s.M.Attempts, id)
			}
		// A promoting attempt is mid-protocol; roll-forward settles it.
		case rec.State != libapi.StatePromoting && !rec.Deadline.IsZero() && now.After(rec.Deadline):
			expired[id] = true
			t = append(t, s.setState(id, libapi.StateDiscarded, "", "deadline passed")...)
		}
	}
	if err := s.save(); err != nil {
		return 0, err
	}
	terminal = t
	return s.removeUnowned(expired)
}

// removeUnowned removes every staged file whose attempt is not registered or is in drop. The caller holds S.mu.
func (s *Store) removeUnowned(drop map[string]bool) (removed int, err error) {
	dir := filepath.Join(s.stateDir(), stagedDirName)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("list %s: %w", dir, err)
	}
	var errs []error
	for _, e := range entries {
		id, _, _ := strings.Cut(e.Name(), ".")
		if _, ok := s.M.Attempts[id]; ok && !drop[id] {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

func (a *Attempts) now() time.Time {
	if a.Now == nil {
		return time.Now()
	}
	return a.Now()
}

// tokenMatches compares the token's hex SHA-256 with hash in constant time.
func tokenMatches(token, hash string) bool {
	sum := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(hash)) == 1
}

// removeFile removes path; a path already gone, or empty, counts as removed.
func removeFile(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// copyIdle copies src to dst until EOF and fails when ctx ends or no bytes arrive for idle. src is read in a goroutine into its own buffer, so a Read that ignores cancellation is abandoned rather than awaited: that goroutine and buffer live on until the Read returns, then exit without touching dst.
func copyIdle(ctx context.Context, dst io.Writer, src io.Reader, idle time.Duration) (int64, error) {
	type chunk struct {
		b   []byte
		err error
	}
	chunks, next, stop := make(chan chunk), make(chan struct{}), make(chan struct{})
	defer close(stop)
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			select {
			case chunks <- chunk{buf[:n], err}:
			case <-stop:
				return
			}
			if err != nil {
				return
			}
			// buf is the consumer's until it asks for the next read.
			select {
			case <-next:
			case <-stop:
				return
			}
		}
	}()

	timer := time.NewTimer(idle)
	defer timer.Stop()
	var written int64
	for {
		select {
		case c := <-chunks:
			if len(c.b) > 0 {
				n, err := dst.Write(c.b)
				written += int64(n)
				if err != nil {
					return written, err
				}
				timer.Reset(idle)
			}
			if errors.Is(c.err, io.EOF) {
				return written, nil
			}
			if c.err != nil {
				return written, c.err
			}
			next <- struct{}{}
		case <-timer.C:
			return written, fmt.Errorf("no bytes for %s: %w", idle, os.ErrDeadlineExceeded)
		case <-ctx.Done():
			return written, ctx.Err()
		}
	}
}
