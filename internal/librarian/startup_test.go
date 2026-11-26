package librarian

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jacaudi/kiwix-operator/internal/netguard"
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

func TestFetchClientRefusesLocalDestinations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a fetch must not reach a loopback server")
	}))
	t.Cleanup(srv.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	resp, err := fetchClient().Do(req)

	if resp != nil {
		assert.NoError(t, resp.Body.Close())
	}
	require.ErrorIs(t, err, netguard.ErrForbidden)
}
