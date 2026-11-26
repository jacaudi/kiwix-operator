package librarian

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var (
	// startTag matches a start tag up to its last well-formed attribute, so a ">" or "/" inside a value never ends it early.
	startTag = regexp.MustCompile(`^<[^\s/>]+(?:\s+[^\s=/>]+\s*=\s*(?:"[^"]*"|'[^']*'))*`)
	// attrPair matches one attribute: submatch 1 is the name, submatch 2 the quoted value.
	attrPair = regexp.MustCompile(`([^\s=/>]+)\s*=\s*("[^"]*"|'[^']*')`)

	attrEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;", "\t", "&#9;", "\n", "&#10;", "\r", "&#13;")
)

// KiwixManage reads a book's metadata by running kiwix-manage against a scratch library.
type KiwixManage struct {
	// Bin is the kiwix-manage executable.
	Bin string
	// ScratchDir holds the throwaway library files; each is removed before Read returns.
	ScratchDir string
}

// Read has kiwix-manage add zimPath to a scratch library and returns the Book from the <book/> element it wrote, with the path rewritten to the absolute zimPath and BookID in dashed lowercase form.
func (k KiwixManage) Read(ctx context.Context, zimPath string) (book Book, err error) {
	abs, err := filepath.Abs(zimPath)
	if err != nil {
		return Book{}, fmt.Errorf("resolve %s: %w", zimPath, err)
	}
	lib := filepath.Join(k.ScratchDir, "lib-"+rand.Text()+".xml")
	defer func() {
		if rmErr := os.Remove(lib); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			book, err = Book{}, errors.Join(err, fmt.Errorf("remove scratch library: %w", rmErr))
		}
	}()

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, k.Bin, lib, "add", abs) //nolint:gosec // Bin is operator configuration and the arguments never reach a shell
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return Book{}, fmt.Errorf("kiwix-manage add %s: %w (stderr: %s)", abs, err, strings.TrimSpace(stderr.String()))
	}
	data, err := os.ReadFile(lib) //nolint:gosec // lib is built from ScratchDir and a random suffix
	if err != nil {
		return Book{}, fmt.Errorf("read scratch library: %w", err)
	}
	return bookFromLibrary(data, abs)
}

// bookFromLibrary builds the Book from the first <book/> element of a library document, rewriting its path to zimPath.
func bookFromLibrary(lib []byte, zimPath string) (Book, error) {
	element, attrs, err := firstBook(lib)
	if err != nil {
		return Book{}, err
	}
	for _, name := range []string{"id", "path", "articleCount", "size"} {
		if attrs[name] == "" {
			return Book{}, fmt.Errorf("book element is missing attribute %q", name)
		}
	}
	articles, err := strconv.ParseInt(attrs["articleCount"], 10, 64)
	if err != nil {
		return Book{}, fmt.Errorf("book attribute %q: %w", "articleCount", err)
	}
	size, err := strconv.ParseInt(attrs["size"], 10, 64)
	if err != nil {
		return Book{}, fmt.Errorf("book attribute %q: %w", "size", err)
	}
	return Book{
		BookID:       strings.ToLower(attrs["id"]),
		Path:         zimPath,
		ArticleCount: articles,
		SizeKiB:      size,
		Element:      RewritePath(element, zimPath),
	}, nil
}

// firstBook returns the source text of the first <book> element in lib, exactly as written, and its attributes.
func firstBook(lib []byte) (element string, attrs map[string]string, err error) {
	dec := xml.NewDecoder(bytes.NewReader(lib))
	var start int64
	depth := 0
	for {
		// InputOffset marks the end of the previous token, which is the start of this one.
		offset := dec.InputOffset()
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return "", nil, errors.New("scratch library has no <book> element")
		}
		if err != nil {
			return "", nil, fmt.Errorf("parse library: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth == 0 && t.Name.Local == "book" {
				start = offset
				attrs = make(map[string]string, len(t.Attr))
				for _, a := range t.Attr {
					attrs[a.Name.Local] = a.Value
				}
			}
			if attrs != nil {
				depth++
			}
		case xml.EndElement:
			if attrs == nil {
				continue
			}
			if depth--; depth == 0 {
				return string(lib[start:dec.InputOffset()]), attrs, nil
			}
		}
	}
}

// RewritePath replaces the value of the path attribute of element's start tag with the XML-escaped newPath. Every other byte of element, including child elements, is kept as written; an element without a path attribute comes back unchanged.
func RewritePath(element, newPath string) string {
	tag := startTag.FindString(element)
	for _, m := range attrPair.FindAllStringSubmatchIndex(tag, -1) {
		if tag[m[2]:m[3]] == "path" {
			// m[4] and m[5] bound the quoted value, quotes included.
			return element[:m[4]+1] + attrEscaper.Replace(newPath) + element[m[5]-1:]
		}
	}
	return element
}
