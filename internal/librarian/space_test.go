package librarian

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFreeBytes(t *testing.T) {
	dir := t.TempDir()

	free, err := FreeBytes(dir)

	require.NoError(t, err)
	assert.Positive(t, free)
	_, err = FreeBytes(filepath.Join(dir, "missing"))
	require.Error(t, err)
}
