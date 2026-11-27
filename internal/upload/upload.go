// Package upload is the crawl pod's last step: it sends the finished ZIM, and optionally a tar of the WARCs, to the librarian, retrying so a restarted kiwix pod does not lose the crawl.
package upload

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/jacaudi/kiwix-operator/internal/libclient"
)

const (
	// initialBackoff is the first wait after a failed PUT; each wait doubles up to Config.MaxBackoff.
	initialBackoff = 5 * time.Second
	// warcsTar is the tar of the WARCs, written next to the ZIM.
	warcsTar = "warcs.tar"
)

// Config says what to upload and how long to keep trying.
type Config struct {
	// OutputDir holds exactly one *.zim and, for KeepWarcs, the crawl's build dir.
	OutputDir string
	// Attempt is the upload attempt's ID and Token its upload token.
	Attempt, Token string
	// KeepWarcs also uploads a tar of the crawl's WARCs as part "warcs".
	KeepWarcs bool
	// Budget is the total backoff sleep for the whole run, shared by every part and excluding transfer time; MaxBackoff caps each single wait.
	Budget, MaxBackoff time.Duration
	// Sleep waits between attempts; nil waits on the context's clock, so a signal ends the wait.
	Sleep func(time.Duration)
	// Logger receives one line per retry; nil discards.
	Logger *slog.Logger
}

// Validate reports every problem with c at once.
func (c Config) Validate() error {
	var errs []error
	if c.OutputDir == "" {
		errs = append(errs, errors.New("config OutputDir is required"))
	}
	if c.Attempt == "" {
		errs = append(errs, errors.New("config Attempt is required"))
	}
	if c.Token == "" {
		errs = append(errs, errors.New("config Token is required"))
	}
	if c.Budget < 0 {
		errs = append(errs, fmt.Errorf("config Budget %s must not be negative", c.Budget))
	}
	// A zero cap would turn the backoff into a busy loop.
	if c.MaxBackoff <= 0 {
		errs = append(errs, fmt.Errorf("config MaxBackoff %s must be positive", c.MaxBackoff))
	}
	return errors.Join(errs...)
}

// part is one file the librarian receives, with the SHA-256 and length computed once before its first PUT.
type part struct {
	name, path, sha string
	length          int64
}

// Run uploads the ZIM, then the WARC tar when cfg.KeepWarcs is set. Every part is staged and hashed before the first PUT, so a missing archive fails before hours of upload.
func Run(ctx context.Context, cfg Config, c *libclient.Client) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	zim, err := findZim(cfg.OutputDir)
	if err != nil {
		return err
	}
	parts := []part{{name: "zim", path: zim}}
	if cfg.KeepWarcs {
		tarPath, err := ensureWarcsTar(cfg.OutputDir)
		if err != nil {
			return err
		}
		parts = append(parts, part{name: "warcs", path: tarPath})
	}
	for i := range parts {
		if parts[i].sha, parts[i].length, err = digest(parts[i].path); err != nil {
			return fmt.Errorf("hash part %s: %w", parts[i].name, err)
		}
	}

	u := uploader{cfg: cfg, client: c, log: cfg.Logger, sleep: cfg.Sleep}
	if u.log == nil {
		u.log = slog.New(slog.DiscardHandler)
	}
	if u.sleep == nil {
		u.sleep = func(d time.Duration) { sleepCtx(ctx, d) }
	}
	for _, p := range parts {
		if err := u.put(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

type uploader struct {
	cfg    Config
	client *libclient.Client
	log    *slog.Logger
	sleep  func(time.Duration)
	// slept is the backoff spent so far by every part of the run, held against Config.Budget.
	slept time.Duration
}

// put sends p, backing off between failures that a repeat may cure, until the budget is spent.
func (u *uploader) put(ctx context.Context, p part) error {
	delay := min(initialBackoff, u.cfg.MaxBackoff)
	for n := 1; ; n++ {
		retry, err := u.putOnce(ctx, p)
		if err == nil {
			return nil
		}
		if !retry || ctx.Err() != nil {
			return fmt.Errorf("put part %s: %w", p.name, err)
		}
		if u.slept >= u.cfg.Budget {
			return fmt.Errorf("put part %s: gave up after %d attempts and %s of backoff: %w", p.name, n, u.slept, err)
		}
		// Warn, not Info: a retry is a recoverable fault, and the ledgered ruling asks for one line each.
		u.log.Warn("upload failed, retrying", "part", p.name, "attempt", n, "backoff", delay, "error", err)
		u.sleep(delay)
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("put part %s: %w", p.name, err)
		}
		u.slept += delay
		delay = min(delay*2, u.cfg.MaxBackoff)
	}
}

// putOnce sends p from the start of its file. retry reports whether the failure is worth repeating; a file that cannot be opened is not.
func (u *uploader) putOnce(ctx context.Context, p part) (retry bool, err error) {
	f, err := os.Open(p.path)
	if err != nil {
		return false, err
	}
	defer f.Close() //nolint:errcheck // read-only
	err = u.client.PutPart(ctx, u.cfg.Attempt, p.name, u.cfg.Token, p.sha, p.length, f)
	return err != nil && libclient.Retryable(err), err
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// findZim returns the one regular *.zim file in dir.
func findZim(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("list %s: %w", dir, err)
	}
	var zims []string
	for _, e := range entries {
		if e.Type().IsRegular() && filepath.Ext(e.Name()) == ".zim" {
			zims = append(zims, filepath.Join(dir, e.Name()))
		}
	}
	if len(zims) != 1 {
		return "", fmt.Errorf("found %d .zim files in %s, want exactly 1", len(zims), dir)
	}
	return zims[0], nil
}

// digest returns the lowercase hex SHA-256 and the length of the file at path.
func digest(path string) (sha string, length int64, err error) {
	f, err := os.Open(path) //nolint:gosec // path is the ZIM or tar found under the output dir from the flags
	if err != nil {
		return "", 0, err
	}
	defer f.Close() //nolint:errcheck // read-only
	h := sha256.New()
	length, err = io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), length, nil
}

// ensureWarcsTar returns the path of dir's warcs.tar, building it first unless it is already there.
func ensureWarcsTar(dir string) (string, error) {
	path := filepath.Join(dir, warcsTar)
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return path, nil
	case !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("check %s: %w", path, err)
	}
	if err := buildWarcsTar(dir, path); err != nil {
		return "", fmt.Errorf("build %s: %w", path, err)
	}
	return path, nil
}

// buildWarcsTar tars every build/collections/*/archive under dir into dest. Only the archive dirs go in: the crawler leaves root-owned 0700 profile dirs beside them that uid 1000 cannot read. The tar is written to a temp file and renamed, so a crash never leaves a truncated warcs.tar for the skip-if-exists rule to upload.
func buildWarcsTar(dir, dest string) error {
	archives, err := filepath.Glob(filepath.Join(dir, "build", "collections", "*", "archive"))
	if err != nil {
		return err
	}
	archives = slices.DeleteFunc(archives, func(p string) bool {
		info, err := os.Stat(p)
		return err != nil || !info.IsDir()
	})
	if len(archives) == 0 {
		return fmt.Errorf("no WARC archive dirs under %s", filepath.Join(dir, "build", "collections"))
	}
	// Sorted, so every build of the same crawl is the same bytes.
	slices.Sort(archives)

	tmp := dest + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // tmp is the output dir from the flags plus a fixed name
	if err != nil {
		return err
	}
	werr := writeTar(f, dir, archives)
	if cerr := f.Close(); werr != nil || cerr != nil {
		return errors.Join(werr, cerr, os.Remove(tmp))
	}
	return os.Rename(tmp, dest)
}

// writeTar writes the directories and regular files under each archive dir to f as entries named relative to root, then fsyncs f.
func writeTar(f *os.File, root string, archives []string) error {
	tw := tar.NewWriter(f)
	for _, archive := range archives {
		err := filepath.WalkDir(archive, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			// Symlinks and special files are not WARCs.
			if !d.IsDir() && !d.Type().IsRegular() {
				return nil
			}
			return addEntry(tw, root, path, d)
		})
		if err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return f.Sync()
}

// addEntry writes path, named relative to root, and its contents when it is a file.
func addEntry(tw *tar.Writer, root, path string, d fs.DirEntry) error {
	info, err := d.Info()
	if err != nil {
		return err
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return err
	}
	hdr.Name = filepath.ToSlash(rel)
	if d.IsDir() {
		hdr.Name += "/"
	}
	// Owners are dropped so the same crawl tars to the same bytes on any host.
	hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if d.IsDir() {
		return nil
	}
	src, err := os.Open(path) //nolint:gosec // path comes from walking an archive dir under the output dir
	if err != nil {
		return err
	}
	defer src.Close() //nolint:errcheck // read-only
	_, err = io.Copy(tw, src)
	return err
}
