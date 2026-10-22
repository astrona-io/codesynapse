//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package config

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestLoadRejectsFIFO(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, syscall.Mkfifo(filepath.Join(root, Filename), 0600))
	_, err := Load(context.Background(), root)
	require.Error(t, err)
}
