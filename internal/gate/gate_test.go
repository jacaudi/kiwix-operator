package gate_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jacaudi/kiwix-operator/api/v1alpha1"
	"github.com/jacaudi/kiwix-operator/internal/gate"
)

func TestUnchanged(t *testing.T) {
	tests := []struct {
		name string
		in   gate.PreCheckInput
		want bool
	}{
		{"catalog same version", gate.PreCheckInput{HasCurrent: true, Kind: "catalog", CurrentFingerprint: "catalog:a/maxi", SpecFingerprint: "catalog:a/maxi", CurrentVersion: "2026-10", CandidateVersion: "2026-10"}, true},
		{"catalog older candidate", gate.PreCheckInput{HasCurrent: true, Kind: "catalog", CurrentFingerprint: "catalog:a/maxi", SpecFingerprint: "catalog:a/maxi", CurrentVersion: "2026-10", CandidateVersion: "2026-09"}, true},
		{"catalog newer candidate", gate.PreCheckInput{HasCurrent: true, Kind: "catalog", CurrentFingerprint: "catalog:a/maxi", SpecFingerprint: "catalog:a/maxi", CurrentVersion: "2026-10", CandidateVersion: "2026-11"}, false},
		{"fingerprint mismatch", gate.PreCheckInput{HasCurrent: true, Kind: "catalog", CurrentFingerprint: "catalog:a/maxi", SpecFingerprint: "catalog:a/nopic", CurrentVersion: "2026-10", CandidateVersion: "2026-10"}, false},
		{"no current", gate.PreCheckInput{Kind: "catalog", CurrentFingerprint: "catalog:a/maxi", SpecFingerprint: "catalog:a/maxi", CurrentVersion: "2026-10", CandidateVersion: "2026-10"}, false},
		{"url same sha", gate.PreCheckInput{HasCurrent: true, Kind: "url", CurrentFingerprint: "url:u", SpecFingerprint: "url:u", CurrentSHA: "abc", SpecSHA: "abc"}, true},
		{"url different sha", gate.PreCheckInput{HasCurrent: true, Kind: "url", CurrentFingerprint: "url:u", SpecFingerprint: "url:u", CurrentSHA: "abc", SpecSHA: "def"}, false},
		{"url no spec sha", gate.PreCheckInput{HasCurrent: true, Kind: "url", CurrentFingerprint: "url:u", SpecFingerprint: "url:u", CurrentSHA: "", SpecSHA: ""}, false},
		{"crawl never unchanged", gate.PreCheckInput{HasCurrent: true, Kind: "crawl", CurrentFingerprint: "crawl:x", SpecFingerprint: "crawl:x"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, gate.Unchanged(tt.in))
		})
	}
}

func TestDecideFirstVersion(t *testing.T) {
	tests := []struct {
		name        string
		articles    int64
		wantPromote bool
		wantResult  v1alpha1.AttemptResult
		wantMessage string
	}{
		{"one article promotes", 1, true, v1alpha1.AttemptResultSucceeded, ""},
		{"zero articles rejected", 0, false, v1alpha1.AttemptResultRejected, "0 pages in new version; nothing to promote"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := gate.Decide(gate.Input{Staged: gate.Counts{ArticleCount: tt.articles, SizeKiB: 10}, MinArticleRatio: 80, MinSizeRatio: 80})
			assert.Equal(t, tt.wantPromote, got.Promote)
			assert.Equal(t, tt.wantResult, got.Result)
			assert.Equal(t, tt.wantMessage, got.Message)
		})
	}
}

func TestDecideRatios(t *testing.T) {
	cur := &gate.Counts{ArticleCount: 3531, SizeKiB: 6835}
	d := gate.Decide(gate.Input{Current: cur, Staged: gate.Counts{ArticleCount: 12, SizeKiB: 40}, MinArticleRatio: 80, MinSizeRatio: 80})
	assert.False(t, d.Promote)
	assert.Equal(t, v1alpha1.AttemptResultRejected, d.Result)
	assert.Equal(t, "12 pages vs 3,531 current (needs 80%); kept current", d.Message)

	ok := gate.Decide(gate.Input{Current: cur, Staged: gate.Counts{ArticleCount: 3540, SizeKiB: 6900}, MinArticleRatio: 80, MinSizeRatio: 80})
	assert.True(t, ok.Promote)
	assert.Equal(t, v1alpha1.AttemptResultSucceeded, ok.Result)
}

func TestDecideRatioBoundary(t *testing.T) {
	cur := &gate.Counts{ArticleCount: 100, SizeKiB: 1000}
	in := gate.Input{Current: cur, MinArticleRatio: 80, MinSizeRatio: 80}

	in.Staged = gate.Counts{ArticleCount: 80, SizeKiB: 800}
	assert.True(t, gate.Decide(in).Promote, "exactly the ratio passes")

	in.Staged = gate.Counts{ArticleCount: 79, SizeKiB: 800}
	assert.False(t, gate.Decide(in).Promote, "one page short fails")

	in.Staged = gate.Counts{ArticleCount: 80, SizeKiB: 799}
	assert.False(t, gate.Decide(in).Promote, "one KiB short fails")
}

func TestDecideSizeRatioCatchesBanner(t *testing.T) {
	cur := &gate.Counts{ArticleCount: 3531, SizeKiB: 6835}
	d := gate.Decide(gate.Input{Current: cur, Staged: gate.Counts{ArticleCount: 3531, SizeKiB: 683}, MinArticleRatio: 80, MinSizeRatio: 80})
	assert.False(t, d.Promote)
	assert.Equal(t, v1alpha1.AttemptResultRejected, d.Result)
	assert.Equal(t, "size 683 KiB vs 6,835 KiB current (needs 80%); kept current", d.Message)
}

func TestDecideArticlesCheckedBeforeSize(t *testing.T) {
	cur := &gate.Counts{ArticleCount: 3531, SizeKiB: 6835}
	d := gate.Decide(gate.Input{Current: cur, Staged: gate.Counts{ArticleCount: 12, SizeKiB: 1}, MinArticleRatio: 80, MinSizeRatio: 80})
	assert.Equal(t, "12 pages vs 3,531 current (needs 80%); kept current", d.Message)
}

func TestDecideZeroRatiosAcceptSmaller(t *testing.T) {
	cur := &gate.Counts{ArticleCount: 3531, SizeKiB: 6835}
	d := gate.Decide(gate.Input{Current: cur, Staged: gate.Counts{ArticleCount: 1, SizeKiB: 1}})
	assert.True(t, d.Promote)
	assert.Equal(t, v1alpha1.AttemptResultSucceeded, d.Result)
}

func TestDecideURLUnchangedAfterDownload(t *testing.T) {
	cur := &gate.Counts{ArticleCount: 3531, SizeKiB: 6835}
	d := gate.Decide(gate.Input{
		Current: cur, Staged: gate.Counts{ArticleCount: 3531, SizeKiB: 6835}, MinArticleRatio: 80, MinSizeRatio: 80,
		URLWithoutSpecSHA: true, CurrentSHA: "abc", StagedSHA: "abc",
	})
	assert.False(t, d.Promote)
	assert.Equal(t, v1alpha1.AttemptResultUnchanged, d.Result)
}

func TestDecideURLChangedAfterDownload(t *testing.T) {
	cur := &gate.Counts{ArticleCount: 3531, SizeKiB: 6835}
	d := gate.Decide(gate.Input{
		Current: cur, Staged: gate.Counts{ArticleCount: 3540, SizeKiB: 6900}, MinArticleRatio: 80, MinSizeRatio: 80,
		URLWithoutSpecSHA: true, CurrentSHA: "abc", StagedSHA: "def",
	})
	assert.True(t, d.Promote)
}
