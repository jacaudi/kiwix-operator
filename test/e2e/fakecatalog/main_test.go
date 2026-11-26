package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jacaudi/kiwix-operator/internal/catalog"
)

func TestCatalogClientResolvesTheServedBook(t *testing.T) {
	file := filepath.Join("..", "..", "fixtures", "zim", "book-01.zim")
	data, err := os.ReadFile(file)
	require.NoError(t, err)
	h, err := newHandler(file)
	require.NoError(t, err)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	got, err := (&catalog.Client{BaseURL: srv.URL}).Latest(t.Context(), "any_name", "")
	require.NoError(t, err)
	sum := sha256.Sum256(data)
	assert.Equal(t, catalog.Entry{
		Name:    "any_name",
		File:    "book-01.zim",
		Version: "2026-10",
		URL:     srv.URL + "/files/book-01.zim",
		SHA256:  hex.EncodeToString(sum[:]),
		Size:    int64(len(data)),
	}, got)

	resp, err := http.Get(got.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, data, body)
}
