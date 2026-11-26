package librarian

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jacaudi/kiwix-operator/internal/libapi"
	"github.com/jacaudi/kiwix-operator/internal/zim"
)

// BookReader produces the Book for a ZIM file, in production by running kiwix-manage.
type BookReader interface {
	Read(ctx context.Context, zimPath string) (Book, error)
}

// Scan reconciles m with the top-level .zim files in dataDir, disk winning and canonical files claiming book IDs first: unreadable and duplicate-ID files go to unmanaged and stay out of m.Books, pending unlinks are skipped, and m changes only when err is nil.
func Scan(ctx context.Context, dataDir string, r BookReader, m *Manifest) (unmanaged []libapi.UnmanagedFile, changed bool, err error) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil, false, fmt.Errorf("scan %s: %w", dataDir, err)
	}
	books := make(map[string]Book)
	archives := make(map[string]ArchiveCurrent)
	owner := make(map[string]string) // book ID -> file that holds it

	for _, name := range candidates(entries, m.PendingUnlinks) {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		path := filepath.Join(dataDir, name)

		id, err := zim.ReadUUID(path)
		if err != nil {
			unmanaged = append(unmanaged, libapi.UnmanagedFile{Name: name, Error: err.Error()})
			continue
		}
		if first, dup := owner[id]; dup {
			unmanaged = append(unmanaged, libapi.UnmanagedFile{Name: name, Error: "duplicate book id of " + first})
			continue
		}

		book, err := bookFor(ctx, r, m, path, id)
		if err != nil {
			unmanaged = append(unmanaged, libapi.UnmanagedFile{Name: name, Error: err.Error()})
			continue
		}
		books[name] = book
		owner[id] = name

		if archive, ok := zim.IsCanonicalFile(name); ok {
			current, ok := m.Archives[archive]
			if !ok || current.BookID != id {
				// Provenance belongs to the replaced file, so a new file starts without it.
				current = ArchiveCurrent{File: name, BookID: id, ArticleCount: book.ArticleCount, SizeKiB: book.SizeKiB}
			}
			archives[archive] = current
		}
	}

	changed = !maps.Equal(books, m.Books) || !maps.Equal(archives, m.Archives)
	m.Books, m.Archives = books, archives
	return unmanaged, changed, nil
}

// bookFor returns the manifest's entry when it already records id for this file, so kiwix-manage only runs for new or replaced files.
func bookFor(ctx context.Context, r BookReader, m *Manifest, path, id string) (Book, error) {
	if book, ok := m.Books[filepath.Base(path)]; ok && book.BookID == id {
		return book, nil
	}
	return readBook(ctx, r, path, id)
}

// readBook runs the reader on path and requires it to agree with id, the book ID in the file's header.
func readBook(ctx context.Context, r BookReader, path, id string) (Book, error) {
	book, err := r.Read(ctx, path)
	if err != nil {
		return Book{}, err
	}
	if book.BookID != id {
		return Book{}, fmt.Errorf("book id %s from the reader does not match header id %s", book.BookID, id)
	}
	book.Path = path
	return book, nil
}

// candidates lists the scannable .zim names, canonical files first so a copy sorting earlier cannot take an archive's book ID; each class is in name order.
func candidates(entries []os.DirEntry, pending []string) []string {
	var names []string
	for _, e := range entries {
		name := e.Name()
		// DirEntry.Type comes from lstat semantics: symlinks and directories are not regular.
		if e.Type().IsRegular() && strings.HasSuffix(name, ".zim") && !slices.Contains(pending, name) {
			names = append(names, name)
		}
	}
	// os.ReadDir sorts by name, and a stable sort keeps that order within each class.
	slices.SortStableFunc(names, func(a, b string) int { return cmp.Compare(rank(a), rank(b)) })
	return names
}

func rank(name string) int {
	if _, ok := zim.IsCanonicalFile(name); ok {
		return 0
	}
	return 1
}
