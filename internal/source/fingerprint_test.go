package source_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jacaudi/kiwix-operator/api/v1alpha1"
	"github.com/jacaudi/kiwix-operator/internal/source"
)

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestFingerprint(t *testing.T) {
	seeds := []string{"https://b/", "https://a/"}
	tests := []struct {
		name string
		src  v1alpha1.SourceSpec
		want string
	}{
		{
			name: "crawl hashes sorted seeds",
			src:  v1alpha1.SourceSpec{Crawl: &v1alpha1.CrawlSource{Seeds: seeds}},
			want: "crawl:" + sha256hex("https://a/,https://b/"),
		},
		{
			name: "catalog with flavour",
			src:  v1alpha1.SourceSpec{Download: &v1alpha1.DownloadSource{Catalog: &v1alpha1.CatalogRef{Name: "wikipedia_en_all", Flavour: "maxi"}}},
			want: "catalog:wikipedia_en_all/maxi",
		},
		{
			name: "catalog without flavour",
			src:  v1alpha1.SourceSpec{Download: &v1alpha1.DownloadSource{Catalog: &v1alpha1.CatalogRef{Name: "stardewvalleywiki_en_all"}}},
			want: "catalog:stardewvalleywiki_en_all/",
		},
		{
			name: "url",
			src:  v1alpha1.SourceSpec{Download: &v1alpha1.DownloadSource{URL: "https://x/y.zim"}},
			want: "url:https://x/y.zim",
		},
		{
			name: "empty spec",
			src:  v1alpha1.SourceSpec{},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, source.Fingerprint(tt.src))
		})
	}
}

func TestFingerprintDoesNotReorderSeeds(t *testing.T) {
	seeds := []string{"https://b/", "https://a/"}
	source.Fingerprint(v1alpha1.SourceSpec{Crawl: &v1alpha1.CrawlSource{Seeds: seeds}})
	assert.Equal(t, []string{"https://b/", "https://a/"}, seeds)
}
