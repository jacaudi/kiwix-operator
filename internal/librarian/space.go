package librarian

import (
	"fmt"
	"io"

	"golang.org/x/sys/unix"
)

// spaceCheckEvery is how many bytes a part writes between free-space re-checks.
const spaceCheckEvery = 64 << 20

// FreeBytes returns the bytes an unprivileged process can still write to the filesystem holding dir.
func FreeBytes(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", dir, err)
	}
	// Bsize is uint32 on darwin and int64 on linux, so one uint64 product keeps every conversion needed on both.
	return int64(st.Bavail * uint64(st.Bsize)), nil //nolint:gosec // G115: a filesystem's available bytes overflow int64 only past 8 EiB
}

// checkSpace fails with ErrInsufficientSpace unless writing need more bytes leaves Reserve free.
func (a *Attempts) checkSpace(need int64) error {
	free, err := a.S.freeBytes()
	if err != nil {
		return fmt.Errorf("measure free space: %w", err)
	}
	if free < need+a.Reserve {
		return fmt.Errorf("%d bytes free, %d needed plus the %d-byte reserve: %w", free, need, a.Reserve, ErrInsufficientSpace)
	}
	return nil
}

// spaceWriter passes writes to w and re-checks free space each time another spaceCheckEvery bytes have gone through; left is what remains to be written.
type spaceWriter struct {
	w           io.Writer
	a           *Attempts
	left, since int64
}

func (sw *spaceWriter) Write(p []byte) (int, error) {
	n, err := sw.w.Write(p)
	sw.left -= int64(n)
	sw.since += int64(n)
	if err == nil && sw.since >= spaceCheckEvery {
		sw.since = 0
		// Floored at 0, so a body that outruns its estimate still keeps the reserve.
		err = sw.a.checkSpace(max(sw.left, 0))
	}
	return n, err
}
