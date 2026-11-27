package zim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fixturesDir = "../../test/fixtures/zim"

func TestReadUUIDMatchesFixtures(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(fixturesDir, "fixtures.json"))
	require.NoError(t, err)
	var entries []struct {
		File string `json:"file"`
		UUID string `json:"uuid"`
	}
	require.NoError(t, json.Unmarshal(raw, &entries))
	require.NotEmpty(t, entries)

	for _, e := range entries {
		t.Run(e.File, func(t *testing.T) {
			got, err := ReadUUID(filepath.Join(fixturesDir, e.File))
			require.NoError(t, err)
			assert.Equal(t, e.UUID, got)
		})
	}
}

func TestReadUUIDRejectsNonZim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zeros.zim")
	require.NoError(t, os.WriteFile(path, make([]byte, 100), 0o600))

	_, err := ReadUUID(path)
	assert.ErrorContains(t, err, "not a ZIM")
}

func TestReadUUIDShortFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "short.zim")
	require.NoError(t, os.WriteFile(path, []byte("ZIM\x04\x00\x00\x00\x00\x00\x00"), 0o600))

	_, err := ReadUUID(path)
	assert.Error(t, err)
}

func TestValidArchiveName(t *testing.T) {
	for _, ok := range []string{"a", "arm-reference", strings.Repeat("a", 45)} {
		assert.True(t, ValidArchiveName(ok), ok)
	}
	for _, bad := range []string{"", "Arm", "-a", "a-", "a_b", "a.b", strings.Repeat("a", 46)} {
		assert.False(t, ValidArchiveName(bad), bad)
	}
}

func TestValidAdoptFileName(t *testing.T) {
	for _, ok := range []string{"arm.jonpalmisc.com_en_all_2026-11_2026-11.zim", "bg3.wiki_0e9fc406.zim", "A.zim"} {
		assert.True(t, ValidAdoptFileName(ok), ok)
	}
	for _, bad := range []string{".hidden.zim", "a/b.zim", "a.ZIM", "a.zim\x00", "a", "../a.zim", "a b.zim"} {
		assert.False(t, ValidAdoptFileName(bad), bad)
	}
}

func TestNormalizeVersion(t *testing.T) {
	cases := map[string]string{
		"devdocs_en_go_2026-10.zim":     "2026-10",
		"wikipedia_en_100_full_2026-11": "2026-11",
		"2026-10-31":                    "2026-10",
		"2026-10-31T00:00:00Z":          "2026-10",
	}
	for in, want := range cases {
		got, ok := NormalizeVersion(in)
		assert.True(t, ok, in)
		assert.Equal(t, want, got, in)
	}
	for _, bad := range []string{"bg3.wiki_0e9fc406.zim", "", "2026-1"} {
		_, ok := NormalizeVersion(bad)
		assert.False(t, ok, bad)
	}
}

func TestIsCanonicalFile(t *testing.T) {
	a, ok := IsCanonicalFile("arm-reference.zim")
	assert.True(t, ok)
	assert.Equal(t, "arm-reference", a)
	for _, bad := range []string{"wikipedia_en_100_2025-10.zim", "Arm.zim", "a.ZIM", "a.zim.part", strings.Repeat("a", 50) + ".zim"} {
		_, ok := IsCanonicalFile(bad)
		assert.False(t, ok, bad)
	}
}

func TestContentID(t *testing.T) {
	cases := map[string]string{
		"arm-reference.zim":    "arm-reference",
		"/data/with space.zim": "with_space",
		"/data/Foo Bar.zim":    "foo_bar",
		"c++.zim":              "cplusplus",
		"Café_X.zim":           "cafe_x",
	}
	for in, want := range cases {
		assert.Equal(t, want, ContentID(in), in)
	}
}
