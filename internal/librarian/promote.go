package librarian

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jacaudi/kiwix-operator/api/v1alpha1"
	"github.com/jacaudi/kiwix-operator/internal/libapi"
	"github.com/jacaudi/kiwix-operator/internal/zim"
)

const (
	stateDirName  = ".librarian"
	stagedDirName = "staged"
	warcsDirName  = "warcs"

	partZIM   = "zim"
	partWARCs = "warcs"

	opPromote = "promote"
	opAdopt   = "adopt"
)

var (
	// ErrCurrentChanged means the archive's current book is not the one the request expected.
	ErrCurrentChanged = errors.New("current book changed")
	// ErrDuplicateBook means another file already serves the book.
	ErrDuplicateBook = errors.New("duplicate book")
	// ErrAdoptInvalid means the adoptFile name or file is not adoptable.
	ErrAdoptInvalid = errors.New("adoptFile invalid")
	// ErrAdoptMissing means the adoptFile does not exist.
	ErrAdoptMissing = errors.New("adoptFile missing")
	// ErrNotFound means the attempt is not registered.
	ErrNotFound = errors.New("not found")
	// ErrConflict means the request does not fit the attempt's or archive's state.
	ErrConflict = errors.New("conflict")
)

// Store owns the manifest and every move of a file in the data directory. Build it with NewStore. mu guards M and is taken before W's lock; under it the only I/O is local files, kiwix-manage and W.Write.
type Store struct {
	DataDir string
	M       *Manifest
	W       *Writer
	R       BookReader
	// OnTerminal, when set, is called with each attempt that became promoted, discarded or failed, after mu is released.
	OnTerminal func(attempt string)
	// Now stamps each attempt change; nil means time.Now.
	Now func() time.Time
	// Free measures the free bytes of the filesystem holding a directory; nil means FreeBytes.
	Free func(dir string) (int64, error)

	mu          sync.Mutex
	unconfirmed bool
	unmanaged   []libapi.UnmanagedFile
	snap        atomic.Pointer[Snapshot]
}

// Snapshot is an immutable copy of what the read endpoints serve, published after every manifest change; it shares nothing with the manifest. Library.FreeBytes is measured at publish time.
type Snapshot struct {
	Archives map[string]libapi.BookInfo
	Attempts map[string]libapi.AttemptState
	Library  libapi.Library
}

// NewStore returns a Store over m with its first snapshot published.
func NewStore(dataDir string, m *Manifest, w *Writer, r BookReader) *Store {
	s := &Store{DataDir: dataDir, M: m, W: w, R: r}
	s.publish()
	return s
}

// Snapshot returns the latest published snapshot without taking the lock.
func (s *Store) Snapshot() *Snapshot { return s.snap.Load() }

// Current returns the archive's current book, or nil when it has none.
func (s *Store) Current(archive string) *libapi.BookInfo {
	if b, ok := s.Snapshot().Archives[archive]; ok {
		return &b
	}
	return nil
}

// Promote makes a staged attempt's book the archive's current one if the current book is still req.ExpectedCurrentBookID. A repeat for a promoted attempt returns the current book.
func (s *Store) Promote(ctx context.Context, attempt string, req libapi.PromoteRequest) (libapi.BookInfo, error) {
	var terminal []string
	defer func() { s.notify(terminal) }()
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, ok := s.M.Attempts[attempt]
	switch {
	case !ok:
		return libapi.BookInfo{}, fmt.Errorf("attempt %s: %w", attempt, ErrNotFound)
	case rec.State == libapi.StatePromoted:
		return s.info(rec.Archive), nil
	case rec.State != libapi.StateStaged || rec.Staged == nil:
		return libapi.BookInfo{}, fmt.Errorf("attempt %s is %s, not staged: %w", attempt, rec.State, ErrConflict)
	}
	in, err := s.intentFor(opPromote, rec.Archive, rec.Staged.Path, req.ExpectedCurrentBookID, *rec.Staged)
	if err != nil {
		return libapi.BookInfo{}, err
	}
	in.Attempt = attempt
	in.Current.SHA256 = rec.Parts[partZIM].SHA256
	in.Current.SourceVersion, in.Current.SourceFingerprint = req.SourceVersion, req.SourceFingerprint
	s.setState(attempt, libapi.StatePromoting, "", "")
	if err := s.begin(in); err != nil {
		return libapi.BookInfo{}, err
	}
	// From here the intent is on disk, so a client hanging up must not stop the protocol half way.
	if terminal, err = s.finish(context.WithoutCancel(ctx), in, true); err != nil {
		return libapi.BookInfo{}, err
	}
	return s.info(rec.Archive), nil
}

// Adopt renames an unmanaged top-level file to the archive's file and makes it current, keeping its book ID.
func (s *Store) Adopt(ctx context.Context, archive string, req libapi.AdoptRequest) (libapi.BookInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	src, err := s.adoptSource(req.File)
	if err != nil {
		return libapi.BookInfo{}, err
	}
	id, err := zim.ReadUUID(src)
	if err != nil {
		return libapi.BookInfo{}, fmt.Errorf("adoptFile %s: %w: %w", req.File, ErrAdoptInvalid, err)
	}
	book, err := bookFor(ctx, s.R, s.M, src, id)
	if err != nil {
		return libapi.BookInfo{}, fmt.Errorf("adoptFile %s: %w: %w", req.File, ErrAdoptInvalid, err)
	}
	in, err := s.intentFor(opAdopt, archive, src, req.ExpectedCurrentBookID, book)
	if err != nil {
		return libapi.BookInfo{}, err
	}
	in.Current.SourceVersion = adoptVersion(req.File, book.Element)
	in.Current.SourceFingerprint = req.SourceFingerprint
	if err := s.begin(in); err != nil {
		return libapi.BookInfo{}, err
	}
	if _, err := s.finish(context.WithoutCancel(ctx), in, true); err != nil {
		return libapi.BookInfo{}, err
	}
	return s.info(archive), nil
}

// Delete discards the archive's open attempts and forgets all of them. Retain keeps the file, which stays the archive's by name. Delete removes the book and unlinks the file and WARC tar once kiwix-serve confirms the book is gone; done is false while that is pending.
func (s *Store) Delete(ctx context.Context, archive, policy string) (done bool, err error) {
	if policy != string(v1alpha1.DeletionPolicyRetain) && policy != string(v1alpha1.DeletionPolicyDelete) {
		return false, fmt.Errorf("deletion policy %q: want %s or %s", policy, v1alpha1.DeletionPolicyRetain, v1alpha1.DeletionPolicyDelete)
	}
	var terminal []string
	defer func() { s.notify(terminal) }()
	s.mu.Lock()
	defer s.mu.Unlock()

	terminal, err = s.dropAttempts(archive)
	if err != nil {
		return false, err
	}
	// A pending intent would otherwise bring the archive back on the next roll-forward.
	s.M.Intents = slices.DeleteFunc(s.M.Intents, func(in Intent) bool { return in.Archive == archive })
	if policy == string(v1alpha1.DeletionPolicyRetain) {
		return true, s.save()
	}

	file := archive + ".zim"
	var c Change
	if cur, ok := s.M.Archives[archive]; ok {
		c.Absent = []string{cur.BookID}
	}
	delete(s.M.Archives, archive)
	delete(s.M.Books, file)
	// Recorded before the write, so a crash before the unlink leaves it pending rather than served again.
	if !slices.Contains(s.M.PendingUnlinks, file) {
		s.M.PendingUnlinks = append(s.M.PendingUnlinks, file)
	}
	if err := s.save(); err != nil {
		return false, err
	}
	if _, err := s.write(ctx, c); err != nil {
		return false, err
	}
	return !slices.Contains(s.M.PendingUnlinks, file), nil
}

// Rescan reconciles the manifest with the data directory and writes library.xml with what changed.
func (s *Store) Rescan(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	before := maps.Clone(s.M.Books)
	unmanaged, changed, err := Scan(ctx, s.DataDir, s.R, s.M)
	if err != nil {
		return err
	}
	s.unmanaged = unmanaged
	if changed {
		if err := s.save(); err != nil {
			return err
		}
	}
	_, err = s.write(ctx, bookChanges(before, s.M.Books))
	return err
}

// RollForward finishes or drops the intents a crash left behind.
func (s *Store) RollForward(ctx context.Context) error {
	var terminal []string
	defer func() { s.notify(terminal) }()
	s.mu.Lock()
	defer s.mu.Unlock()

	// One intent that cannot complete must not hold back the others, so every error is collected.
	var errs []error
	for _, in := range slices.Clone(s.M.Intents) {
		t, err := s.rollForward(ctx, in)
		terminal = append(terminal, t...)
		if err != nil {
			errs = append(errs, fmt.Errorf("roll forward %s of %s: %w", in.Op, in.Archive, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Store) rollForward(ctx context.Context, in Intent) ([]string, error) {
	if s.M.Archives[in.Archive].BookID != in.ExpectedCurrentBookID {
		return s.dropIntent(in)
	}
	switch _, err := os.Lstat(in.Source); {
	case err == nil:
		return s.finish(ctx, in, true)
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("stat %s: %w", in.Source, err)
	}
	if id, err := zim.ReadUUID(s.target(in.Archive)); err == nil && id == in.NewBookID {
		return s.finish(ctx, in, false)
	}
	return s.dropIntent(in)
}

// dropIntent abandons an intent; its attempt goes back to staged while the source remains, and fails otherwise.
func (s *Store) dropIntent(in Intent) ([]string, error) {
	s.M.Intents = slices.DeleteFunc(s.M.Intents, func(i Intent) bool { return i.Archive == in.Archive })
	var terminal []string
	if in.Attempt != "" {
		if _, err := os.Lstat(in.Source); err == nil {
			s.setState(in.Attempt, libapi.StateStaged, "", "")
		} else {
			terminal = s.setState(in.Attempt, libapi.StateFailed, libapi.CodeInternal, "promotion could not be resumed after a restart")
		}
	}
	return terminal, s.save()
}

// intentFor makes every check a promotion or adoption needs before anything is written: no intent pending for the archive, the compare-and-swap, a rename that can succeed, no other file serving the book, and a path rewrite that took.
func (s *Store) intentFor(op, archive, source, expected string, book Book) (Intent, error) {
	if slices.ContainsFunc(s.M.Intents, func(in Intent) bool { return in.Archive == archive }) {
		return Intent{}, fmt.Errorf("archive %s has a promotion in progress; retry once it completes: %w", archive, ErrConflict)
	}
	if cur := s.M.Archives[archive].BookID; cur != expected {
		return Intent{}, fmt.Errorf("archive %s: current book is %q, expected %q: %w", archive, cur, expected, ErrCurrentChanged)
	}
	target := s.target(archive)
	if err := renameable(source, target); err != nil {
		return Intent{}, err
	}
	for file, b := range s.M.Books {
		if path := filepath.Join(s.DataDir, file); b.BookID == book.BookID && path != target && path != source {
			return Intent{}, fmt.Errorf("book %s is already served by %s; set adoptFile: %s to adopt that file instead: %w", book.BookID, file, file, ErrDuplicateBook)
		}
	}
	book.Path, book.Element = target, RewritePath(book.Element, target)
	if !strings.Contains(book.Element, `path="`+attrEscaper.Replace(target)+`"`) {
		return Intent{}, fmt.Errorf("rewrite path of book %s to %s: its element has no path attribute to rewrite", book.BookID, target)
	}
	return Intent{
		Op: op, Archive: archive, Source: source, ExpectedCurrentBookID: expected, NewBookID: book.BookID, Book: book,
		Current: ArchiveCurrent{File: filepath.Base(target), BookID: book.BookID, ArticleCount: book.ArticleCount, SizeKiB: book.SizeKiB},
	}, nil
}

// renameable refuses a rename that would fail after the intent is saved: the source must be a regular file, and the target absent or a regular file.
func renameable(source, target string) error {
	switch st, err := os.Lstat(source); {
	case err != nil:
		return fmt.Errorf("stat source %s: %w", source, err)
	case !st.Mode().IsRegular():
		return fmt.Errorf("source %s is not a regular file", source)
	}
	switch st, err := os.Lstat(target); {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("stat %s: %w", target, err)
	case !st.Mode().IsRegular():
		return fmt.Errorf("%s exists and is not a regular file; remove it so the archive can be promoted", target)
	}
	return nil
}

// begin saves the intent, clearing any pending unlink of the archive's file in the same save (protocol step 2).
func (s *Store) begin(in Intent) error {
	s.M.Intents = append(s.M.Intents, in)
	s.M.PendingUnlinks = slices.DeleteFunc(s.M.PendingUnlinks, func(name string) bool { return name == in.Current.File })
	return s.save()
}

// finish runs protocol steps 3 to 5 of a saved intent: rename the source over the archive's file, write library.xml, commit. A promotion's kept WARC tar moves right after the rename, so every path to the commit, roll-forward included, keeps it.
func (s *Store) finish(ctx context.Context, in Intent, rename bool) ([]string, error) {
	target := s.target(in.Archive)
	if rename {
		if err := os.Rename(in.Source, target); err != nil {
			return nil, fmt.Errorf("move %s over %s: %w", in.Source, target, err)
		}
		if err := syncDir(s.DataDir); err != nil {
			return nil, err
		}
	}
	if in.Attempt != "" && slices.Contains(s.M.Attempts[in.Attempt].RegisterRequest.Parts, partWARCs) {
		if err := s.moveWarcs(in.Attempt, in.Archive); err != nil {
			return nil, fmt.Errorf("attempt %s: keep its WARCs: %w", in.Attempt, err)
		}
	}
	c := Change{Present: map[string]string{in.NewBookID: zim.ContentID(target)}}
	if old := s.M.Archives[in.Archive].BookID; old != "" && old != in.NewBookID {
		c.Absent = []string{old}
	}
	if in.Op == opAdopt {
		delete(s.M.Books, filepath.Base(in.Source))
	}
	s.M.Books[filepath.Base(target)] = in.Book
	// An unconfirmed write still commits: the file is in place and the next confirmed write reloads it.
	if _, err := s.write(ctx, c); err != nil {
		return nil, err
	}
	s.M.Archives[in.Archive] = in.Current
	s.M.Intents = slices.DeleteFunc(s.M.Intents, func(i Intent) bool { return i.Archive == in.Archive })
	var terminal []string
	if in.Attempt != "" {
		terminal = s.setState(in.Attempt, libapi.StatePromoted, "", "")
	}
	return terminal, s.save()
}

// write renders library.xml and, once kiwix-serve confirms it, unlinks every pending file no served book uses. Each pending file's book ID joins c.Absent unless a served book has it, so the confirmation proves those books are gone. The caller holds mu.
func (s *Store) write(ctx context.Context, c Change) (bool, error) {
	served := make(map[string]bool, len(s.M.Books))
	for _, b := range s.M.Books {
		served[b.BookID] = true
	}
	for _, name := range s.M.PendingUnlinks {
		if id, err := zim.ReadUUID(filepath.Join(s.DataDir, name)); err == nil && !served[id] && !slices.Contains(c.Absent, id) {
			c.Absent = append(c.Absent, id)
		}
	}
	confirmed, err := s.W.Write(ctx, s.M, c)
	if err != nil {
		return false, err
	}
	s.unconfirmed = !confirmed
	if !confirmed || len(s.M.PendingUnlinks) == 0 {
		s.publish()
		return confirmed, nil
	}
	var errs []error
	s.M.PendingUnlinks = slices.DeleteFunc(s.M.PendingUnlinks, func(name string) bool {
		// A file served again is no longer to be unlinked, and leaving it listed would hide it from the next scan.
		if _, ok := s.M.Books[name]; ok {
			return true
		}
		if err := s.unlink(name); err != nil {
			errs = append(errs, err)
			return false
		}
		return true
	})
	return true, errors.Join(append(errs, s.save())...)
}

// unlink removes a top-level file and, for an archive's file, the archive's WARC tar; a file already gone counts as removed.
func (s *Store) unlink(name string) error {
	paths := []string{filepath.Join(s.DataDir, name)}
	if archive, ok := zim.IsCanonicalFile(name); ok {
		paths = append(paths, s.warcsPath(archive))
	}
	var errs []error
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// dropAttempts discards the archive's open attempts, removes their staged files and forgets every attempt of the archive.
func (s *Store) dropAttempts(archive string) (terminal []string, err error) {
	var errs []error
	for id, rec := range s.M.Attempts {
		if rec.Archive != archive {
			continue
		}
		if !terminalState(rec.State) {
			terminal = append(terminal, s.setState(id, libapi.StateDiscarded, "", "archive deleted")...)
		}
		errs = append(errs, s.removeStaged(id))
		delete(s.M.Attempts, id)
	}
	return terminal, errors.Join(errs...)
}

// removeStaged removes every staged file of the attempt: parts, partials and WARCs.
func (s *Store) removeStaged(attempt string) error {
	paths, err := filepath.Glob(s.stagedPath(attempt, ".*"))
	if err != nil {
		return fmt.Errorf("list staged files of %s: %w", attempt, err)
	}
	var errs []error
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// moveWarcs replaces the archive's WARC tar with the attempt's staged one; a staged tar already gone was moved by an earlier pass of the same promotion.
func (s *Store) moveWarcs(attempt, archive string) error {
	src := s.stagedPath(attempt, "."+partWARCs)
	if _, err := os.Lstat(src); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	dst := s.warcsPath(archive)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(dst), err)
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("move WARCs of %s: %w", attempt, err)
	}
	return syncDir(filepath.Dir(dst))
}

// setState moves an attempt to state and returns it when that made it terminal. The caller holds mu, saves, and passes the result to notify after unlocking.
func (s *Store) setState(attempt, state, code, msg string) (terminal []string) {
	rec, ok := s.M.Attempts[attempt]
	if !ok {
		return nil
	}
	was := terminalState(rec.State)
	rec.State, rec.Code, rec.Error, rec.UpdatedAt = state, code, msg, s.now()
	s.M.Attempts[attempt] = rec
	if !was && terminalState(state) {
		return []string{attempt}
	}
	return nil
}

func terminalState(state string) bool {
	return state == libapi.StatePromoted || state == libapi.StateDiscarded || state == libapi.StateFailed
}

func (s *Store) notify(terminal []string) {
	if s.OnTerminal == nil {
		return
	}
	for _, attempt := range terminal {
		s.OnTerminal(attempt)
	}
}

// save writes the manifest and publishes a snapshot. When the save fails, M is reloaded from disk so memory never runs ahead of the file.
func (s *Store) save() error {
	err := s.M.Save(s.stateDir())
	if err != nil {
		m, _, loadErr := LoadManifest(s.stateDir())
		if loadErr != nil {
			return errors.Join(err, loadErr)
		}
		*s.M = *m
	}
	s.publish()
	return err
}

// publish builds a deep copy of the read-side state and swaps it in. The caller holds mu.
func (s *Store) publish() {
	snap := &Snapshot{
		Archives: make(map[string]libapi.BookInfo, len(s.M.Archives)),
		Attempts: make(map[string]libapi.AttemptState, len(s.M.Attempts)),
		Library: libapi.Library{
			Books:             make([]libapi.BookInfo, 0, len(s.M.Books)),
			Unmanaged:         slices.Clone(s.unmanaged),
			ReloadUnconfirmed: s.unconfirmed,
		},
	}
	// A failed measurement reports 0 rather than failing the change it publishes.
	if free, err := s.freeBytes(); err == nil {
		snap.Library.FreeBytes = free
	}
	for archive := range s.M.Archives {
		snap.Archives[archive] = s.info(archive)
	}
	for _, file := range slices.Sorted(maps.Keys(s.M.Books)) {
		if archive, ok := zim.IsCanonicalFile(file); ok && s.M.Archives[archive].File == file {
			snap.Library.Books = append(snap.Library.Books, snap.Archives[archive])
			continue
		}
		b := s.M.Books[file]
		snap.Library.Books = append(snap.Library.Books, libapi.BookInfo{File: file, BookID: b.BookID, ArticleCount: b.ArticleCount, SizeKiB: b.SizeKiB})
	}
	for id, rec := range s.M.Attempts {
		st := libapi.AttemptState{Archive: rec.Archive, Attempt: id, Kind: rec.Kind, State: rec.State, Code: rec.Code, Error: rec.Error}
		if b := rec.Staged; b != nil {
			st.Staged = &libapi.BookInfo{BookID: b.BookID, ArticleCount: b.ArticleCount, SizeKiB: b.SizeKiB, SHA256: rec.Parts[partZIM].SHA256}
		}
		if cur, ok := snap.Archives[rec.Archive]; ok {
			st.Current = &cur
		}
		snap.Attempts[id] = st
	}
	s.snap.Store(snap)
}

// attempt returns the attempt's state from the snapshot the caller, holding mu, just published.
func (s *Store) attempt(id string) libapi.AttemptState { return s.snap.Load().Attempts[id] }

func (s *Store) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

func (s *Store) freeBytes() (int64, error) {
	if s.Free == nil {
		return FreeBytes(s.DataDir)
	}
	return s.Free(s.DataDir)
}

func (s *Store) info(archive string) libapi.BookInfo {
	c := s.M.Archives[archive]
	return libapi.BookInfo{
		Archive: archive, File: c.File, BookID: c.BookID, ArticleCount: c.ArticleCount, SizeKiB: c.SizeKiB,
		SHA256: c.SHA256, SourceVersion: c.SourceVersion, SourceFingerprint: c.SourceFingerprint,
	}
}

// adoptSource validates an adoptFile name and returns its path: a basename, not an archive's file, and an existing regular file (lstat, so no symlinks).
func (s *Store) adoptSource(file string) (string, error) {
	if !zim.ValidAdoptFileName(file) {
		return "", fmt.Errorf("adoptFile %q must be a basename matching ^[A-Za-z0-9][A-Za-z0-9._-]*\\.zim$: %w", file, ErrAdoptInvalid)
	}
	if _, ok := zim.IsCanonicalFile(file); ok {
		return "", fmt.Errorf("adoptFile %s is already named like an archive's file: %w", file, ErrAdoptInvalid)
	}
	path := filepath.Join(s.DataDir, file)
	st, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("adoptFile %s: %w", file, ErrAdoptMissing)
	case err != nil:
		return "", fmt.Errorf("stat adoptFile %s: %w", file, err)
	case !st.Mode().IsRegular():
		return "", fmt.Errorf("adoptFile %s is not a regular file: %w", file, ErrAdoptInvalid)
	}
	return path, nil
}

// adoptVersion is the YYYY-MM in the file name, else the month of the book's date attribute.
func adoptVersion(file, element string) string {
	if v, ok := zim.NormalizeVersion(file); ok {
		return v
	}
	v, _ := zim.NormalizeVersion(attr(element, "date"))
	return v
}

// attr returns the raw value of an attribute of element's start tag, or "" when it has none.
func attr(element, name string) string {
	for _, m := range attrPair.FindAllStringSubmatch(startTag.FindString(element), -1) {
		if m[1] == name {
			return m[2][1 : len(m[2])-1]
		}
	}
	return ""
}

// bookChanges is the Change from one set of served books to the next.
func bookChanges(before, after map[string]Book) Change {
	var c Change
	held := make(map[string]bool, len(after))
	for file, b := range after {
		held[b.BookID] = true
		if old, ok := before[file]; !ok || old.BookID != b.BookID {
			if c.Present == nil {
				c.Present = map[string]string{}
			}
			c.Present[b.BookID] = zim.ContentID(file)
		}
	}
	for _, b := range before {
		if !held[b.BookID] {
			c.Absent = append(c.Absent, b.BookID)
		}
	}
	return c
}

func (s *Store) stateDir() string { return filepath.Join(s.DataDir, stateDirName) }

func (s *Store) target(archive string) string { return filepath.Join(s.DataDir, archive+".zim") }

func (s *Store) stagedPath(attempt, ext string) string {
	return filepath.Join(s.stateDir(), stagedDirName, attempt+ext)
}

func (s *Store) warcsPath(archive string) string {
	return filepath.Join(s.stateDir(), warcsDirName, archive+".tar")
}
