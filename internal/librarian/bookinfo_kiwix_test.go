//go:build kiwix

package librarian

import (
	"context"
	"encoding/json/v2"
	"encoding/xml"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixtureEntry is one record of fixtures.json.
type fixtureEntry struct {
	File         string `json:"file"`
	UUID         string `json:"uuid"`
	ArticleCount int64  `json:"articleCount"`
}

func fixture(t *testing.T, file string) fixtureEntry {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixturesDir, "fixtures.json"))
	require.NoError(t, err)
	var all []fixtureEntry
	require.NoError(t, json.Unmarshal(data, &all))
	for _, f := range all {
		if f.File == file {
			return f
		}
	}
	t.Fatalf("%s is not in fixtures.json", file)
	return fixtureEntry{}
}

func fixturePath(t *testing.T, file string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join(fixturesDir, file))
	require.NoError(t, err)
	return abs
}

func newKiwixManage(t *testing.T) KiwixManage {
	t.Helper()
	return KiwixManage{Bin: "kiwix-manage", ScratchDir: t.TempDir()}
}

func TestKiwixManageReadsFixture(t *testing.T) {
	want := fixture(t, "small-b.zim")
	path := fixturePath(t, "small-b.zim")

	got, err := newKiwixManage(t).Read(t.Context(), path)

	require.NoError(t, err)
	assert.Equal(t, want.UUID, got.BookID)
	assert.Equal(t, want.ArticleCount, got.ArticleCount)
	assert.Equal(t, int64(30), got.ArticleCount)
	assert.Positive(t, got.SizeKiB)
	assert.Equal(t, path, got.Path)
	assert.Contains(t, got.Element, `path="`+path+`"`)
}

func TestKiwixManageKeepsAttributes(t *testing.T) {
	path := fixturePath(t, "small-b.zim")

	got, err := newKiwixManage(t).Read(t.Context(), path)
	require.NoError(t, err)

	// A second, independent library written by kiwix-manage is the reference.
	lib := filepath.Join(t.TempDir(), "reference.xml")
	out, err := exec.CommandContext(t.Context(), "kiwix-manage", lib, "add", path).CombinedOutput()
	require.NoError(t, err, string(out))
	data, err := os.ReadFile(lib)
	require.NoError(t, err)

	want := bookAttrs(t, data)
	have := bookAttrs(t, []byte("<library>"+got.Element+"</library>"))
	require.Contains(t, want, "path")
	delete(want, "path")
	assert.Equal(t, path, have["path"])
	delete(have, "path")
	assert.True(t, maps.Equal(want, have), "attributes differ:\nwant %v\nhave %v", want, have)
	assert.NotEmpty(t, have)
}

// bookAttrs returns the attributes of the <book/> element inside the given document.
func bookAttrs(t *testing.T, doc []byte) map[string]string {
	t.Helper()
	var lib struct {
		Book struct {
			Attrs []xml.Attr `xml:",any,attr"`
		} `xml:"book"`
	}
	require.NoError(t, xml.Unmarshal(doc, &lib))
	attrs := make(map[string]string, len(lib.Book.Attrs))
	for _, a := range lib.Book.Attrs {
		attrs[a.Name.Local] = a.Value
	}
	return attrs
}

func TestKiwixManageRejectsTruncated(t *testing.T) {
	path := fixturePath(t, "truncated.zim")

	_, err := newKiwixManage(t).Read(t.Context(), path)

	require.Error(t, err)
	// kiwix-manage's own stderr travels with the error.
	assert.Contains(t, err.Error(), "Cannot add ZIM")
}

func TestKiwixManageScratchCleaned(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		wantErr bool
	}{
		{name: "after success", file: "small-b.zim"},
		{name: "after kiwix-manage fails", file: "truncated.zim", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			km := newKiwixManage(t)

			_, err := km.Read(t.Context(), fixturePath(t, tt.file))

			assert.Equal(t, tt.wantErr, err != nil)
			entries, readErr := os.ReadDir(km.ScratchDir)
			require.NoError(t, readErr)
			assert.Empty(t, entries)
		})
	}
}

func TestKiwixManageCancelledContext(t *testing.T) {
	km := newKiwixManage(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := km.Read(ctx, fixturePath(t, "small-b.zim"))

	require.Error(t, err)
	entries, readErr := os.ReadDir(km.ScratchDir)
	require.NoError(t, readErr)
	assert.Empty(t, entries)
}
