package ingest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestRunReportsProgressInOrder(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "app.js"), []byte("export function f() {}\n"), 0o600))
	var steps []int
	_, err := Run(context.Background(), Options{Repo: "r", Root: root, Progress: func(step int, name string) {
		assert.True(t, name != "")
		steps = append(steps, step)
	}})
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2, 3, 4, 5, 6, 7}, steps)
	assert.Equal(t, Steps, len(steps))
}

func TestRunReportsExtractorTimings(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "app.js"), []byte("export function f() {}\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("# R\n"), 0o600))
	var names []string
	_, err := Run(context.Background(), Options{Repo: "r", Root: root, Extracted: func(extractor string, d time.Duration) {
		assert.True(t, d >= 0)
		names = append(names, extractor)
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{"javascript", "docs", "config"}, names, "only languages present, then docs and config")
}
