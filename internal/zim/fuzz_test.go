package zim

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func FuzzReadUUID(f *testing.F) {
	f.Add([]byte("ZIM\x04\x06\x00\x03\x00" + strings.Repeat("u", 16)))
	f.Add([]byte("ZIM\x04"))
	f.Add(make([]byte, 100))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "f.zim")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		_, _ = ReadUUID(path)
	})
}

func FuzzValidAdoptFileName(f *testing.F) {
	for _, s := range []string{"A.zim", "bg3.wiki_0e9fc406.zim", ".hidden.zim", "a/b.zim", "a.zim\x00", "../a.zim", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if ValidAdoptFileName(s) && (strings.Contains(s, "/") || strings.HasPrefix(s, ".")) {
			t.Errorf("ValidAdoptFileName(%q) = true, want false", s)
		}
	})
}
