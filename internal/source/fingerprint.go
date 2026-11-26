// Package source computes the stable fingerprint that tells the manager whether a ZimArchive's source changed.
package source

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	"github.com/jacaudi/kiwix-operator/api/v1alpha1"
)

// Fingerprint returns crawl:<sha256 of sorted seeds joined with ",">, catalog:<name>/<flavour>, or url:<url>; "" when no source is set.
func Fingerprint(s v1alpha1.SourceSpec) string {
	switch {
	case s.Crawl != nil:
		sum := sha256.Sum256([]byte(strings.Join(slices.Sorted(slices.Values(s.Crawl.Seeds)), ",")))
		return "crawl:" + hex.EncodeToString(sum[:])
	case s.Download != nil && s.Download.Catalog != nil:
		return "catalog:" + s.Download.Catalog.Name + "/" + s.Download.Catalog.Flavour
	case s.Download != nil && s.Download.URL != "":
		return "url:" + s.Download.URL
	default:
		return ""
	}
}
