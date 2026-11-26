package catalog

import (
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

const sha256HexLen = 64

// meta4 is the subset of a metalink v4 document that the operator reads.
type meta4 struct {
	Files []struct {
		Name   string `xml:"name,attr"`
		Size   int64  `xml:"size"`
		Hashes []struct {
			Type  string `xml:"type,attr"`
			Value string `xml:",chardata"`
		} `xml:"hash"`
	} `xml:"urn:ietf:params:xml:ns:metalink file"`
}

// ParseMeta4 returns the file name, lowercase hex SHA-256 and size (bytes) of the first file in a metalink v4 document.
func ParseMeta4(r io.Reader) (file, sha256 string, size int64, err error) {
	var m meta4
	if err := xml.NewDecoder(r).Decode(&m); err != nil {
		return "", "", 0, fmt.Errorf("decode meta4: %w", err)
	}
	if len(m.Files) == 0 {
		return "", "", 0, errors.New("meta4 has no file")
	}
	f := m.Files[0]
	if f.Name == "" || f.Size <= 0 {
		return "", "", 0, fmt.Errorf("meta4 file has name %q and size %d, want a name and a positive size", f.Name, f.Size)
	}
	for _, h := range f.Hashes {
		if h.Type != "sha-256" {
			continue
		}
		sum := strings.ToLower(strings.TrimSpace(h.Value))
		if _, err := hex.DecodeString(sum); err != nil || len(sum) != sha256HexLen {
			return "", "", 0, fmt.Errorf("meta4 sha-256 %q is not %d hex characters", sum, sha256HexLen)
		}
		return f.Name, sum, f.Size, nil
	}
	return "", "", 0, fmt.Errorf("meta4 file %q has no sha-256 hash", f.Name)
}
