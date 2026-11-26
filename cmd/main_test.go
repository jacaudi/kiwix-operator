package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestVersionString(t *testing.T) {
	version, commit, date = "v1.2.3", "abc1234", "2026-11-24T00:00:00Z"
	assert.Equal(t, "kiwix-operator v1.2.3 (commit abc1234, built 2026-11-24T00:00:00Z)", versionString())
}

func TestRunVersionFlag(t *testing.T) {
	var out, errb bytes.Buffer
	assert.Equal(t, 0, run([]string{"--version"}, &out, &errb))
	assert.True(t, strings.HasPrefix(out.String(), "kiwix-operator "))
}

func TestRunUnknownSubcommand(t *testing.T) {
	var out, errb bytes.Buffer
	assert.Equal(t, 2, run([]string{"bogus"}, &out, &errb))
	assert.Contains(t, errb.String(), "usage: kiwix-operator <manager|librarian|upload>")
}

func TestRunNoArgs(t *testing.T) {
	var out, errb bytes.Buffer
	assert.Equal(t, 2, run(nil, &out, &errb))
}

func TestLibrarianRequiresToken(t *testing.T) {
	t.Setenv("KIWIX_LIBRARIAN_ADMIN_TOKEN", "")
	var out, errb bytes.Buffer

	assert.Equal(t, 2, run([]string{"librarian"}, &out, &errb))
	assert.Contains(t, errb.String(), "KIWIX_LIBRARIAN_ADMIN_TOKEN is required")
}

func TestLibrarianRejectsBadFlags(t *testing.T) {
	t.Setenv("KIWIX_LIBRARIAN_ADMIN_TOKEN", "token")
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"unknown flag", []string{"librarian", "--bogus"}, "bogus"},
		{"bad log format", []string{"librarian", "--log-format=xml"}, "log format"},
		{"bad log level", []string{"librarian", "--log-level=loud"}, "log-level"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer

			assert.Equal(t, 2, run(tc.args, &out, &errb))
			assert.Contains(t, errb.String(), tc.wantErr)
		})
	}
}
