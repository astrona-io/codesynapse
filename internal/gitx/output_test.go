package gitx

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestBoundedOutput(t *testing.T) {
	for _, tc := range []struct {
		name     string
		chunks   []string
		want     string
		overflow bool
	}{
		{"exact", []string{"ab", "cd"}, "abcd", false},
		{"single oversized write", []string{"abcde"}, "", true},
		{"cumulative overflow", []string{"abc", "de"}, "abc", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := boundedOutput{limit: 4}
			var err error
			for _, chunk := range tc.chunks {
				_, err = b.Write([]byte(chunk))
				if err != nil {
					break
				}
			}
			if tc.overflow {
				require.ErrorIs(t, err, ErrOutputLimit)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, b.buffer.String(), tc.want)
			assert.Equal(t, b.overflow, tc.overflow)
		})
	}
}

func TestRunRejectsOversizedOutput(t *testing.T) {
	dir := t.TempDir()
	_, err := Run(t.Context(), dir, "init", "-q")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "large"), bytes.Repeat([]byte{'x'}, MaxDiffBytes+1), 0600))
	out, err := Run(t.Context(), dir, "hash-object", "-w", "--", "large")
	require.NoError(t, err)
	out, err = Run(t.Context(), dir, "cat-file", "blob", strings.TrimSpace(string(out)))
	require.ErrorIs(t, err, ErrOutputLimit)
	assert.Len(t, out, 0)
}

func TestRunNeedsCommand(t *testing.T) {
	_, err := Run(t.Context(), t.TempDir())
	require.Error(t, err)
}
