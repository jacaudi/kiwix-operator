package librarian

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// WriteFileAtomic replaces path with data: temp file in the same directory, fsync, optional mtime, rename, then fsync of the directory. No temp file survives an error.
func WriteFileAtomic(path string, data []byte, mtime *time.Time) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", path, err)
	}
	tmp := f.Name()

	if err := writeTemp(f, data); err != nil {
		return errors.Join(fmt.Errorf("write %s: %w", tmp, err), os.Remove(tmp))
	}
	if mtime != nil {
		if err := os.Chtimes(tmp, *mtime, *mtime); err != nil {
			return errors.Join(fmt.Errorf("set mtime of %s: %w", tmp, err), os.Remove(tmp))
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		return errors.Join(fmt.Errorf("rename %s to %s: %w", tmp, path, err), os.Remove(tmp))
	}
	// The rename has happened; a directory sync failure leaves the new file in place, so nothing to remove.
	return syncDir(dir)
}

// writeTemp writes data to f, fsyncs it and closes it, returning every error that occurred.
func writeTemp(f *os.File, data []byte) error {
	_, err := f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // dir is the parent of a path the caller already chose to write
	if err != nil {
		return fmt.Errorf("open directory %s: %w", dir, err)
	}
	return errors.Join(d.Sync(), d.Close())
}
