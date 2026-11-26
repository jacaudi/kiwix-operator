package catalog

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func FuzzParseMeta4(f *testing.F) {
	seed, err := os.ReadFile(filepath.Join("testdata", "devdocs_en_go_2026-10.zim.meta4"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte(`<metalink xmlns="urn:ietf:params:xml:ns:metalink"><file name="a.zim"><size>1</size></file></metalink>`))
	f.Add([]byte("<"))
	f.Add([]byte{})
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _, _, _ = ParseMeta4(bytes.NewReader(data))
	})
}
