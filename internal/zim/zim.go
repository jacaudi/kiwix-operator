// Package zim holds the ZIM-file helpers shared by the manager and the librarian: header UUID, name rules, version normalization and content IDs.
package zim

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"uuid"

	"golang.org/x/text/unicode/norm"
)

const (
	// maxArchiveNameLen keeps derived object names (Jobs, Pods) within the DNS label limit.
	maxArchiveNameLen = 45
	fileExt           = ".zim"
	headerLen         = 24
	uuidOffset        = 8
)

var (
	magic = []byte{0x5a, 0x49, 0x4d, 0x04}

	archiveNameRE   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	adoptFileNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*\.zim$`)
	trailingMonthRE = regexp.MustCompile(`_(\d{4}-(?:0[1-9]|1[0-2]))(?:\.zim)?$`)
	isoDateRE       = regexp.MustCompile(`^(\d{4}-(?:0[1-9]|1[0-2]))-\d{2}`)

	contentIDReplacer = strings.NewReplacer(" ", "_", "+", "plus")
)

// ReadUUID returns the dashed lowercase book ID from the header of the ZIM file at path.
func ReadUUID(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // callers pass paths built from the data directory, never request input
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only

	hdr := make([]byte, headerLen)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return "", fmt.Errorf("read ZIM header of %s: %w", path, err)
	}
	if !bytes.HasPrefix(hdr, magic) {
		return "", fmt.Errorf("%s is not a ZIM file: bad magic number", path)
	}
	return uuid.UUID(hdr[uuidOffset:]).String(), nil
}

// ValidArchiveName reports whether s is a DNS label of 1 to 45 characters.
func ValidArchiveName(s string) bool {
	return len(s) <= maxArchiveNameLen && archiveNameRE.MatchString(s)
}

// ValidAdoptFileName reports whether s is a safe basename for an adoptFile request.
func ValidAdoptFileName(s string) bool {
	return adoptFileNameRE.MatchString(s)
}

// NormalizeVersion extracts a YYYY-MM version from a trailing _YYYY-MM (with or without .zim) or from an ISO date prefix.
func NormalizeVersion(s string) (string, bool) {
	if m := trailingMonthRE.FindStringSubmatch(s); m != nil {
		return m[1], true
	}
	if m := isoDateRE.FindStringSubmatch(s); m != nil {
		return m[1], true
	}
	return "", false
}

// IsCanonicalFile reports whether name is <archive>.zim with a valid archive name, and returns the archive name.
func IsCanonicalFile(name string) (string, bool) {
	archive, ok := strings.CutSuffix(name, fileExt)
	if !ok || !ValidArchiveName(archive) {
		return "", false
	}
	return archive, true
}

// ContentID returns the un-encoded /content/<id> that libkiwix derives from a ZIM file name.
func ContentID(file string) string {
	id := strings.TrimSuffix(filepath.Base(file), fileExt)
	// Mirrors libkiwix removeAccents (stringTools.cpp): "Lower; NFD; [:M:] remove; NFC".
	id = norm.NFD.String(strings.ToLower(id))
	id = strings.Map(func(r rune) rune {
		if unicode.IsMark(r) {
			return -1
		}
		return r
	}, id)
	// Mirrors libkiwix book.cpp: spaces become "_", "+" becomes "plus".
	return contentIDReplacer.Replace(norm.NFC.String(id))
}
