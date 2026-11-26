package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// uploadFixture points the upload subcommand at a fake librarian, a token file and an output dir holding one ZIM.
// The returned slice holds the Authorization header of each PUT the librarian received.
func uploadFixture(t *testing.T, status int) (output string, auths *[]string) {
	t.Helper()
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		got = append(got, r.Header.Get("Authorization"))
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"code":"Conflict","message":"no"}`)
		}
	}))
	t.Cleanup(srv.Close)

	output = t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(output, "docs.zim"), []byte("zim"), 0o644))
	token := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(token, []byte("  secret-token\n"), 0o600))
	old := tokenPath
	tokenPath = token
	t.Cleanup(func() { tokenPath = old })
	t.Setenv("LIBRARIAN_URL", srv.URL)
	t.Setenv("ATTEMPT", "abcdefg2")
	return output, &got
}

func TestUploadSubcommandSendsZim(t *testing.T) {
	output, auths := uploadFixture(t, http.StatusOK)
	var out, errb bytes.Buffer

	code := run([]string{"upload", "--output=" + output}, &out, &errb)

	assert.Equal(t, 0, code, errb.String())
	assert.Equal(t, []string{"Bearer secret-token"}, *auths, "the token file is trimmed")
	assert.NotContains(t, out.String()+errb.String(), "secret-token")
}

func TestUploadSubcommandFailsOnFinalAnswer(t *testing.T) {
	output, auths := uploadFixture(t, http.StatusConflict)
	var out, errb bytes.Buffer

	code := run([]string{"upload", "--output=" + output}, &out, &errb)

	assert.Equal(t, 1, code)
	assert.Len(t, *auths, 1, "a conflict is not retried")
	assert.NotContains(t, out.String()+errb.String(), "secret-token")
}

func TestUploadSubcommandMissingToken(t *testing.T) {
	output, auths := uploadFixture(t, http.StatusOK)
	tokenPath = filepath.Join(t.TempDir(), "missing")
	var out, errb bytes.Buffer

	code := run([]string{"upload", "--output=" + output}, &out, &errb)

	assert.Equal(t, 2, code)
	assert.Contains(t, errb.String(), tokenPath)
	assert.Empty(t, *auths)
}

func TestUploadSubcommandRejectsBadConfig(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T)
		args    []string
		wantErr string
	}{
		{"empty token file", func(t *testing.T) { require.NoError(t, os.WriteFile(tokenPath, []byte(" \n"), 0o600)) }, nil, "empty"},
		{"no LIBRARIAN_URL", func(t *testing.T) { t.Setenv("LIBRARIAN_URL", "") }, nil, "LIBRARIAN_URL is required"},
		{"LIBRARIAN_URL without a scheme", func(t *testing.T) { t.Setenv("LIBRARIAN_URL", "kiwix:8090") }, nil, "LIBRARIAN_URL"},
		{"no ATTEMPT", func(t *testing.T) { t.Setenv("ATTEMPT", "") }, nil, "ATTEMPT is required"},
		{"unknown flag", func(*testing.T) {}, []string{"--bogus"}, "bogus"},
		{"zero max backoff", func(*testing.T) {}, []string{"--max-backoff=0s"}, "MaxBackoff"},
		{"bad log format", func(*testing.T) {}, []string{"--log-format=xml"}, "log format"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			output, auths := uploadFixture(t, http.StatusOK)
			tc.prepare(t)
			var out, errb bytes.Buffer

			code := run(append([]string{"upload", "--output=" + output}, tc.args...), &out, &errb)

			assert.Equal(t, 2, code)
			assert.Contains(t, errb.String(), tc.wantErr)
			assert.Empty(t, *auths)
		})
	}
}

func TestUploadSubcommandReportsEveryConfigProblem(t *testing.T) {
	uploadFixture(t, http.StatusOK)
	t.Setenv("LIBRARIAN_URL", "")
	t.Setenv("ATTEMPT", "")
	var out, errb bytes.Buffer

	code := run([]string{"upload"}, &out, &errb)

	assert.Equal(t, 2, code)
	assert.Contains(t, errb.String(), "LIBRARIAN_URL is required")
	assert.Contains(t, errb.String(), "ATTEMPT is required")
}
