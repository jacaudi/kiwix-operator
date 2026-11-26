package librarian

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/jacaudi/kiwix-operator/internal/libapi"
)

const (
	lockFileName   = "lock"
	scratchDirName = "scratch"
	libraryFile    = "library.xml"

	// The library write routine: poll every second for up to 30 s per render, at most 3 renders.
	confirmPoll    = time.Second
	confirmWindow  = 30 * time.Second
	confirmRenders = 3

	gcInterval = time.Hour
	// fetchHeaderTimeout bounds a fetch's wait for response headers; the body is bounded by idleTimeout instead.
	fetchHeaderTimeout = 60 * time.Second
	// writeTimeout covers a promotion that waits out another confirmed write and then runs its own; a part upload or WARC stream extends its own deadline.
	writeTimeout  = 2 * confirmRenders * confirmWindow
	shutdownGrace = 10 * time.Second
)

// Config is the librarian's configuration, from the librarian subcommand's flags and environment.
type Config struct {
	DataDir, Listen, KiwixServeURL, AdminToken, KiwixManageBin string
	// Reserve is the free space, in bytes, no part or fetch may eat into.
	Reserve int64
	// MaxFetches bounds concurrent fetch downloads.
	MaxFetches int
	// UserAgent is sent on every fetch request.
	UserAgent string
	// Logger receives startup, recovery and shutdown events; nil means slog.Default().
	Logger *slog.Logger
}

func (c Config) validate() error {
	var errs []error
	if c.MaxFetches < 1 {
		errs = append(errs, fmt.Errorf("max fetches is %d; it must be at least 1", c.MaxFetches))
	}
	if c.Reserve < 0 {
		errs = append(errs, fmt.Errorf("reserve is %d bytes; it must not be negative", c.Reserve))
	}
	return errors.Join(errs...)
}

// Run is the librarian process. In order: take the lock, load the manifest (scanning /data when there is none), reset interrupted uploads, render library.xml unconfirmed, mark started and serve; then recover in the background. It returns nil once ctx ends and shutdown completes.
func Run(ctx context.Context, cfg Config) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	log := cmp.Or(cfg.Logger, slog.Default())
	stateDir := filepath.Join(cfg.DataDir, stateDirName)

	lock, err := takeLock(stateDir)
	if err != nil {
		return err
	}
	// Closing the file releases the flock, after everything below has stopped.
	defer lock.Close() //nolint:errcheck // nothing to do if the close fails; the process exit releases it anyway

	km := KiwixManage{Bin: cfg.KiwixManageBin, ScratchDir: filepath.Join(stateDir, scratchDirName)}
	if err := os.MkdirAll(km.ScratchDir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", km.ScratchDir, err)
	}
	m, err := loadOrScan(ctx, cfg.DataDir, stateDir, km)
	if err != nil {
		return err
	}

	w := &Writer{
		Path:      filepath.Join(cfg.DataDir, libraryFile),
		Confirmer: HTTPConfirmer{BaseURL: cfg.KiwixServeURL},
		Now:       time.Now,
		Poll:      confirmPoll, Window: confirmWindow, MaxRenders: confirmRenders,
	}
	store := NewStore(cfg.DataDir, m, w, km)
	attempts := &Attempts{S: store, Reserve: cfg.Reserve, IdleTimeout: idleTimeout}
	fetcher := &Fetcher{A: attempts, HTTP: fetchClient(), UserAgent: cfg.UserAgent, Sem: make(chan struct{}, cfg.MaxFetches)}
	store.OnTerminal = fetcher.Cancel
	attempts.OnFetchRegistered = fetcher.Start

	if err := attempts.ResetReceiving(); err != nil {
		return fmt.Errorf("reset interrupted uploads: %w", err)
	}
	if err := w.WriteUnconfirmed(m); err != nil {
		return fmt.Errorf("render %s: %w", w.Path, err)
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}
	var started atomic.Bool
	srv := &http.Server{
		Handler:           newLibrarianHandler(store, attempts, cfg.AdminToken, started.Load),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	started.Store(true)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	log.InfoContext(ctx, "librarian serving", "addr", ln.Addr().String())

	bg, stopBG := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { recoverAndCollect(bg, log, store, attempts, fetcher) })

	var runErr error
	select {
	case err := <-serveErr:
		runErr = fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		log.InfoContext(ctx, "librarian shutting down")
	}
	// The signal context is already cancelled; WithoutCancel gives in-flight requests the whole grace period.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("shut down the API: %w", err))
	}
	stopBG()
	wg.Wait()
	// After the background work, so no Start follows it; partials stay for the next start to resume.
	fetcher.Stop()
	return runErr
}

// takeLock holds an exclusive flock on <stateDir>/lock, so a second librarian on the same volume refuses to start. Closing the file releases it.
func takeLock(stateDir string) (*os.File, error) {
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		return nil, fmt.Errorf("create %s: %w", stateDir, err)
	}
	path := filepath.Join(stateDir, lockFileName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // path is the state directory from the librarian's flags
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("take lock %s (is another librarian running on this volume?): %w", path, err)
	}
	return f, nil
}

// loadOrScan loads the manifest; with none, a first start or a lost manifest, it scans dataDir synchronously and saves the result, so an adopted library comes up with all its books.
func loadOrScan(ctx context.Context, dataDir, stateDir string, r BookReader) (*Manifest, error) {
	m, existed, err := LoadManifest(stateDir)
	if err != nil || existed {
		return m, err
	}
	if _, _, err := Scan(ctx, dataDir, r, m); err != nil {
		return nil, err
	}
	if err := m.Save(stateDir); err != nil {
		return nil, err
	}
	return m, nil
}

// recoverAndCollect is startup step 6: GC, roll-forward, resuming fetches, and the rescan, then GC hourly until ctx ends. A failing step is logged and the rest still run, so recovery never blocks startup.
func recoverAndCollect(ctx context.Context, log *slog.Logger, store *Store, attempts *Attempts, fetcher *Fetcher) {
	report := func(step string, err error) {
		if err != nil {
			log.ErrorContext(ctx, "startup recovery step failed", "step", step, "error", err)
		}
	}
	gc := func() {
		_, err := attempts.GC()
		report("gc", err)
	}

	gc()
	report("roll forward", store.RollForward(ctx))
	for id, st := range store.Snapshot().Attempts {
		if st.Kind == libapi.KindFetch && (st.State == libapi.StateRegistered || st.State == libapi.StateReceiving) {
			fetcher.Start(id)
		}
	}
	report("rescan", store.Rescan(ctx))
	log.InfoContext(ctx, "startup recovery finished")

	tick := time.NewTicker(gcInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			gc()
		}
	}
}

// fetchClient bounds the wait for response headers, so a server that accepts and stays silent cannot hold a fetch until its 24 h deadline. It sets no total timeout: a download's body is bounded by idleTimeout.
func fetchClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = fetchHeaderTimeout
	return &http.Client{Transport: t}
}

// newLibrarianHandler is NewHandler over the real Store and Attempts: reads come from the snapshot, never S.mu.
func newLibrarianHandler(store *Store, attempts *Attempts, adminToken string, started func() bool) http.Handler {
	lib := func() libapi.Library {
		l := store.Snapshot().Library
		// The snapshot measures free space only when the manifest changes; the manager's LowSpace check needs it now.
		if free, err := store.freeBytes(); err == nil {
			l.FreeBytes = free
		}
		return l
	}
	warcs := func(archive string) (io.ReadCloser, error) {
		f, err := os.Open(store.warcsPath(archive))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil, fmt.Errorf("archive %s has no WARCs: %w", archive, ErrNotFound)
		case err != nil:
			return nil, fmt.Errorf("open WARCs of %s: %w", archive, err)
		}
		return f, nil
	}
	return NewHandler(store, attempts, adminToken, lib, warcs, started)
}
