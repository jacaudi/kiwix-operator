package librarian

import (
	"math"
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

func TestCheckSpace(t *testing.T) {
	tests := []struct {
		name          string
		free, reserve int64
		need          int64
		want          error
	}{
		{name: "fits exactly", free: 150, reserve: 100, need: 50},
		{name: "one byte short", free: 149, reserve: 100, need: 50, want: ErrInsufficientSpace},
		{name: "need overflows the sum", free: 1 << 40, reserve: 100, need: math.MaxInt64 - 1, want: ErrInsufficientSpace},
		{name: "free below the reserve", free: 10, reserve: 100, need: 0, want: ErrInsufficientSpace},
		{name: "negative need", free: 1 << 40, reserve: 100, need: -1, want: ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, ts := newTestAttempts(t)
			a.Reserve = tt.reserve
			ts.Free = func(string) (int64, error) { return tt.free, nil }

			err := a.checkSpace(tt.need)

			if tt.want == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tt.want)
		})
	}
}
