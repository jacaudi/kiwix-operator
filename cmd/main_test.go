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
