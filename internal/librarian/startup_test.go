package librarian

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFetchClientBoundsHeadersOnly(t *testing.T) {
	c := fetchClient()

	assert.Zero(t, c.Timeout, "a download has no total timeout")
	tr, ok := c.Transport.(*http.Transport)
	require.True(t, ok)
	assert.Equal(t, 60*time.Second, tr.ResponseHeaderTimeout)
	assert.NotSame(t, http.DefaultTransport, tr)
	assert.NotNil(t, tr.Proxy, "cloned from http.DefaultTransport")
}

func TestRunRejectsBadConfig(t *testing.T) {
	err := Run(t.Context(), Config{DataDir: t.TempDir(), MaxFetches: 0, Reserve: -1})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "max fetches")
	assert.Contains(t, err.Error(), "reserve")
}
