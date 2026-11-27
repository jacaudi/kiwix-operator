package netguard

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestControl(t *testing.T) {
	tests := []struct {
		address string
		allowed bool
	}{
		{address: "127.0.0.1:80"},
		{address: "[::1]:80"},
		{address: "169.254.169.254:80"},
		{address: "[fe80::1]:80"},
		{address: "[::ffff:169.254.169.254]:80"},
		{address: "0.0.0.0:80"},
		{address: "224.0.0.1:80"},
		{address: "10.96.0.1:443", allowed: true},
		{address: "172.16.0.1:80", allowed: true},
		{address: "192.168.1.1:80", allowed: true},
		{address: "100.64.0.1:80", allowed: true},
		{address: "8.8.8.8:443", allowed: true},
		{address: "[2001:4860:4860::8888]:443", allowed: true},
	}
	for _, tt := range tests {
		t.Run(tt.address, func(t *testing.T) {
			err := Control("tcp", tt.address, nil)

			if tt.allowed {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrForbidden)
		})
	}
}

func TestControlRejectsUnparsableAddress(t *testing.T) {
	require.ErrorIs(t, Control("tcp", "not-an-address", nil), ErrForbidden)
}

func TestTransportRefusesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the request must never reach the server")
	}))
	t.Cleanup(srv.Close)
	c := &http.Client{Transport: Transport()}

	resp, err := c.Get(srv.URL) //nolint:noctx // the dial is refused before any request is sent

	if resp != nil {
		assert.NoError(t, resp.Body.Close())
	}
	require.ErrorIs(t, err, ErrForbidden)
}
