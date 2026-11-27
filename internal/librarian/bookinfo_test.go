package librarian

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRewritePath(t *testing.T) {
	tests := []struct {
		name    string
		element string
		newPath string
		want    string
	}{
		{
			name:    "replaces a relative path",
			element: `<book id="a" path="../x/a.zim" title="T" />`,
			newPath: "/data/a.zim",
			want:    `<book id="a" path="/data/a.zim" title="T" />`,
		},
		{
			name:    "escapes ampersand, quote and less-than",
			element: `<book id="a" path="old" />`,
			newPath: `/data/a&b"c<d>e.zim`,
			want:    `<book id="a" path="/data/a&amp;b&quot;c&lt;d&gt;e.zim" />`,
		},
		{
			name:    "escapes whitespace controls so the parser keeps them",
			element: `<book path="old"/>`,
			newPath: "/data/a\tb\nc\rd.zim",
			want:    `<book path="/data/a&#9;b&#10;c&#13;d.zim"/>`,
		},
		{
			name:    "path is not the first attribute",
			element: `<book id="a" title="T" path="old" size="1" />`,
			newPath: "/data/a.zim",
			want:    `<book id="a" title="T" path="/data/a.zim" size="1" />`,
		},
		{
			name:    "leaves attributes whose names end in path alone",
			element: `<book xpath="1" favicon_path="2" id="a" path="old" pathx="3"/>`,
			newPath: "/data/a.zim",
			want:    `<book xpath="1" favicon_path="2" id="a" path="/data/a.zim" pathx="3"/>`,
		},
		{
			name:    "no path attribute means no change",
			element: `<book xpath="1" favicon_path="2" id="a"/>`,
			newPath: "/data/a.zim",
			want:    `<book xpath="1" favicon_path="2" id="a"/>`,
		},
		{
			name:    "path text inside another value is not an attribute",
			element: `<book title="my path=&quot;x&quot;" path="old"/>`,
			newPath: "/data/a.zim",
			want:    `<book title="my path=&quot;x&quot;" path="/data/a.zim"/>`,
		},
		{
			name:    "single-quoted value keeps its quotes and escapes an apostrophe",
			element: `<book path='old'/>`,
			newPath: "/data/it's.zim",
			want:    `<book path='/data/it&apos;s.zim'/>`,
		},
		{
			name:    "odd spacing around the equals sign is kept",
			element: "<book  id = \"a\"\n  path = \"old\"   />",
			newPath: "/data/a.zim",
			want:    "<book  id = \"a\"\n  path = \"/data/a.zim\"   />",
		},
		{
			name:    "child elements are kept byte for byte",
			element: `<book id="a" path="old"><icon path="child">x&amp;y</icon></book>`,
			newPath: "/data/a.zim",
			want:    `<book id="a" path="/data/a.zim"><icon path="child">x&amp;y</icon></book>`,
		},
		{
			name:    "a child path is not rewritten when the book has none",
			element: `<book id="a"><icon path="child"/></book>`,
			newPath: "/data/a.zim",
			want:    `<book id="a"><icon path="child"/></book>`,
		},
		{
			name:    "empty path value",
			element: `<book path=""/>`,
			newPath: "/data/a.zim",
			want:    `<book path="/data/a.zim"/>`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, RewritePath(tt.element, tt.newPath))
		})
	}
}

// TestRewritePathIsReversible shows the rewrite touches nothing but the path value: writing the old value back restores the element.
func TestRewritePathIsReversible(t *testing.T) {
	element := `<book id="a"  path="../x/a.zim" title="T &amp; U"><icon path="c"/></book>`
	rewritten := RewritePath(element, `/data/a&"<.zim`)
	require.NotEqual(t, element, rewritten)
	assert.Equal(t, element, RewritePath(rewritten, "../x/a.zim"))
}

func TestBookFromLibrary(t *testing.T) {
	const id = "49fc9acf-17ee-c886-d939-d9db2a22162f"
	const book = `<book id="` + id + `" path="../x/a.zim" title="A &amp; B" articleCount="30" size="60" />`
	tests := []struct {
		name    string
		library string
		want    Book
		wantErr string
	}{
		{
			name:    "reads the counts and rewrites the path",
			library: "<?xml version=\"1.0\"?>\n<library version=\"20110515\">\n  " + book + "\n</library>\n",
			want: Book{
				BookID:       id,
				Path:         "/data/a.zim",
				ArticleCount: 30,
				SizeKiB:      60,
				Element:      `<book id="` + id + `" path="/data/a.zim" title="A &amp; B" articleCount="30" size="60" />`,
			},
		},
		{
			name:    "lower-cases the id",
			library: `<library><book id="49FC9ACF-17EE-C886-D939-D9DB2A22162F" path="p" articleCount="1" size="2"/></library>`,
			want: Book{
				BookID:       id,
				Path:         "/data/a.zim",
				ArticleCount: 1,
				SizeKiB:      2,
				Element:      `<book id="49FC9ACF-17EE-C886-D939-D9DB2A22162F" path="/data/a.zim" articleCount="1" size="2"/>`,
			},
		},
		{
			name:    "keeps child elements",
			library: `<library><book id="` + id + `" path="p" articleCount="1" size="2"><x path="c"/></book><!-- later --></library>`,
			want: Book{
				BookID:       id,
				Path:         "/data/a.zim",
				ArticleCount: 1,
				SizeKiB:      2,
				Element:      `<book id="` + id + `" path="/data/a.zim" articleCount="1" size="2"><x path="c"/></book>`,
			},
		},
		{name: "no book", library: `<library version="1"></library>`, wantErr: "no <book> element"},
		{name: "empty library", library: ``, wantErr: "no <book> element"},
		{name: "malformed xml", library: `<library><book id="x"`, wantErr: "parse library"},
		{name: "missing id", library: `<library><book path="p" articleCount="1" size="2"/></library>`, wantErr: `attribute "id"`},
		{name: "missing path", library: `<library><book id="` + id + `" articleCount="1" size="2"/></library>`, wantErr: `attribute "path"`},
		{name: "missing articleCount", library: `<library><book id="` + id + `" path="p" size="2"/></library>`, wantErr: `attribute "articleCount"`},
		{name: "non-numeric size", library: `<library><book id="` + id + `" path="p" articleCount="1" size="big"/></library>`, wantErr: `attribute "size"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := bookFromLibrary([]byte(tt.library), "/data/a.zim")
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// stubManage is a kiwix-manage that runs script, and a KiwixManage over it with the given timeout.
func stubManage(t *testing.T, script string, timeout time.Duration) KiwixManage {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "kiwix-manage")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\n"+script+"\n"), 0o600))
	require.NoError(t, os.Chmod(bin, 0o700))
	return KiwixManage{Bin: bin, ScratchDir: dir, Timeout: timeout}
}

func TestKiwixManageReadTimesOut(t *testing.T) {
	tests := []struct {
		name   string
		script string
	}{
		{name: "hung child", script: "sleep 30"},
		{name: "grandchild holds stderr open", script: "sleep 30 &\nsleep 30"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := stubManage(t, tt.script, 200*time.Millisecond)
			start := time.Now()

			_, err := k.Read(t.Context(), filepath.Join(t.TempDir(), "a.zim"))

			require.Error(t, err)
			assert.Less(t, time.Since(start), 10*time.Second, "kiwix-manage must be killed at the deadline, not awaited")
		})
	}
}
