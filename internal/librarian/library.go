package librarian

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"sync"
	"time"
)

// libraryOpen is the root element kiwix-serve expects.
const libraryOpen = `<library version="20110515">`

// Render returns library.xml for m: every book's element in file-name order.
func Render(m *Manifest) []byte {
	var b bytes.Buffer
	b.WriteString(libraryOpen)
	for _, file := range slices.Sorted(maps.Keys(m.Books)) {
		b.WriteString(m.Books[file].Element)
	}
	b.WriteString("</library>\n")
	return b.Bytes()
}

// Writer serializes writes of library.xml. kiwix-serve --monitorLibrary compares whole-second mtimes with a strict >, so every write must land at least one second after the file it replaces. Now, Poll, Window and MaxRenders must be set; MaxRenders is at least 1.
type Writer struct {
	Path      string
	Confirmer Confirmer
	Now       func() time.Time
	// Poll is the confirmation interval, Window how long one render is polled, MaxRenders the total renders (the first included) before giving up.
	Poll, Window time.Duration
	MaxRenders   int

	mu sync.Mutex
}

// Write renders m and polls the Confirmer until it confirms c, re-rendering when a Window passes unconfirmed, up to MaxRenders renders. It returns false with a nil error when none was confirmed; a Confirm error counts as unconfirmed, and a cancelled ctx returns its error.
func (w *Writer) Write(ctx context.Context, m *Manifest, c Change) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for range w.MaxRenders {
		if err := w.render(m); err != nil {
			return false, err
		}
		confirmed, err := w.await(ctx, c)
		if err != nil || confirmed {
			return confirmed, err
		}
	}
	return false, nil
}

// WriteUnconfirmed renders m with the same mtime rule and does not wait for kiwix-serve.
func (w *Writer) WriteUnconfirmed(m *Manifest) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.render(m)
}

// render writes m at max(now, previous mtime + 1s), both truncated to the second; a missing file counts as written at now. Callers hold w.mu.
func (w *Writer) render(m *Manifest) error {
	now := w.Now().Truncate(time.Second)
	prev := now
	switch st, err := os.Stat(w.Path); {
	case err == nil:
		prev = st.ModTime().Truncate(time.Second)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("stat %s: %w", w.Path, err)
	}
	mtime := now
	if floor := prev.Add(time.Second); floor.After(mtime) {
		mtime = floor
	}
	return WriteFileAtomic(w.Path, Render(m), &mtime)
}

// await polls the Confirmer every Poll for one Window.
func (w *Writer) await(ctx context.Context, c Change) (bool, error) {
	window := time.NewTimer(w.Window)
	defer window.Stop()
	tick := time.NewTicker(w.Poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-window.C:
			return false, nil
		case <-tick.C:
			// An error means the answer is unknown, so keep polling until the window ends.
			if ok, err := w.Confirmer.Confirm(ctx, c); err == nil && ok {
				return true, nil
			}
		}
	}
}
