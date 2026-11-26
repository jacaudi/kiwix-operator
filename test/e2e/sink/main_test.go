package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jacaudi/kiwix-operator/internal/notify"
)

func post(t *testing.T, url, body string) int {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	return resp.StatusCode
}

func received(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url + "/received")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body)
}

func TestSinkRecordsNotificationsInOrder(t *testing.T) {
	srv := httptest.NewServer(newHandler())
	t.Cleanup(srv.Close)

	assert.JSONEq(t, `[]`, received(t, srv.URL))
	assert.Equal(t, http.StatusNoContent, post(t, srv.URL+"/hooks/kiwix", `{"title":"a updated","message":"3 pages"}`))
	assert.Equal(t, http.StatusNoContent, post(t, srv.URL+"/hooks/kiwix", `{"title":"a refresh rejected","message":"kept"}`))
	assert.JSONEq(t, `[{"title":"a updated","message":"3 pages"},{"title":"a refresh rejected","message":"kept"}]`, received(t, srv.URL))
}

func TestSinkAcceptsTheOperatorsNotifier(t *testing.T) {
	srv := httptest.NewServer(newHandler())
	t.Cleanup(srv.Close)

	title, msg := notify.Rejected("site", "kept current")
	require.NoError(t, (&notify.Notifier{}).Send(t.Context(), srv.URL+"/hooks/kiwix", title, msg))
	assert.JSONEq(t, `[{"title":"site refresh rejected","message":"kept current"}]`, received(t, srv.URL))
}

func TestSinkRejectsOffContractBodies(t *testing.T) {
	tests := []struct {
		name, body string
	}{
		{"not JSON", `title=a`},
		{"unknown member", `{"title":"a","message":"m","extra":1}`},
		{"no title", `{"message":"m"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(newHandler())
			t.Cleanup(srv.Close)
			assert.Equal(t, http.StatusBadRequest, post(t, srv.URL+"/hooks/kiwix", tt.body))
			assert.JSONEq(t, `[]`, received(t, srv.URL))
		})
	}
}
