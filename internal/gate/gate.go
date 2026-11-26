// Package gate decides whether a new ZIM is unchanged and whether a staged ZIM is healthy enough to promote.
package gate

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/jacaudi/kiwix-operator/api/v1alpha1"
)

// Counts are the measurements of one ZIM that the health gate compares.
type Counts struct {
	ArticleCount, SizeKiB int64
}

// PreCheckInput is what the pre-download check knows about the current version and the candidate source.
type PreCheckInput struct {
	HasCurrent                                                                                       bool
	CurrentFingerprint, SpecFingerprint, CurrentVersion, CandidateVersion, CurrentSHA, SpecSHA, Kind string
}

// Unchanged reports whether the source has nothing newer than the current version, so no download is needed.
func Unchanged(in PreCheckInput) bool {
	if !in.HasCurrent || in.CurrentFingerprint != in.SpecFingerprint {
		return false
	}
	switch in.Kind {
	case "catalog":
		return in.CandidateVersion <= in.CurrentVersion
	case "url":
		return in.SpecSHA != "" && in.SpecSHA == in.CurrentSHA
	default:
		return false
	}
}

// Input is what the health gate compares; a nil Current means this is the first version.
type Input struct {
	Current                       *Counts
	Staged                        Counts
	MinArticleRatio, MinSizeRatio int32
	CurrentSHA, StagedSHA         string
	URLWithoutSpecSHA             bool
}

// Decision is the gate's verdict; Message is empty unless the staged version was refused or unchanged.
type Decision struct {
	Promote bool
	Result  v1alpha1.AttemptResult
	Message string
}

// Decide applies the health gate to a staged ZIM.
func Decide(in Input) Decision {
	if in.URLWithoutSpecSHA && in.StagedSHA != "" && in.StagedSHA == in.CurrentSHA {
		return Decision{Result: v1alpha1.AttemptResultUnchanged, Message: "download matches current; nothing to promote"}
	}
	if in.Current == nil {
		if in.Staged.ArticleCount < 1 {
			return rejected("0 pages in new version; nothing to promote")
		}
		return promoted()
	}
	if !meetsRatio(in.Staged.ArticleCount, in.Current.ArticleCount, in.MinArticleRatio) {
		return rejected(fmt.Sprintf("%s pages vs %s current (needs %d%%); kept current",
			formatInt(in.Staged.ArticleCount), formatInt(in.Current.ArticleCount), in.MinArticleRatio))
	}
	if !meetsRatio(in.Staged.SizeKiB, in.Current.SizeKiB, in.MinSizeRatio) {
		return rejected(fmt.Sprintf("size %s KiB vs %s KiB current (needs %d%%); kept current",
			formatInt(in.Staged.SizeKiB), formatInt(in.Current.SizeKiB), in.MinSizeRatio))
	}
	return promoted()
}

func promoted() Decision {
	return Decision{Promote: true, Result: v1alpha1.AttemptResultSucceeded}
}

func rejected(msg string) Decision {
	return Decision{Result: v1alpha1.AttemptResultRejected, Message: msg}
}

// meetsRatio is staged >= current*ratio/100 in integer arithmetic, so there is no float rounding.
func meetsRatio(staged, current int64, ratio int32) bool {
	return staged*100 >= current*int64(ratio)
}

// formatInt renders n with comma thousands separators.
func formatInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}
