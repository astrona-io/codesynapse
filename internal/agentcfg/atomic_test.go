package agentcfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// Hooks of several sessions change settings.json together: every
// read-change-write sees the one before it, and the file is never half
// written.
func TestSettingsLock(t *testing.T) {
	file := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, WritePrivate(file, []byte("0")))
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			assert.NoError(t, withFileLock(file, func() error {
				b, err := os.ReadFile(file) //nolint:gosec // test file
				if err != nil {
					return err
				}
				n, err := strconv.Atoi(string(b))
				if err != nil {
					return err
				}
				return WritePrivate(file, []byte(strconv.Itoa(n+1)))
			}))
		})
	}
	wg.Wait()
	b, err := os.ReadFile(file) //nolint:gosec // test file
	require.NoError(t, err)
	assert.Equal(t, string(b), "20")
	info, err := os.Stat(file)
	require.NoError(t, err)
	assert.Equal(t, info.Mode().Perm(), os.FileMode(0o600))
	_, err = os.Stat(file + ".codesynapse.lock")
	assert.True(t, os.IsNotExist(err), "the lock is released")
}

func TestSettingsLockStale(t *testing.T) {
	file := filepath.Join(t.TempDir(), "settings.json")
	lock := file + ".codesynapse.lock"
	require.NoError(t, os.WriteFile(lock, nil, 0o600))
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(lock, old, old))
	ran := false
	require.NoError(t, withFileLock(file, func() error { ran = true; return nil }))
	assert.True(t, ran, "a lock left by a dead process is taken over")
}

func TestPointClaudeGatewayConcurrent(t *testing.T) {
	file := filepath.Join(t.TempDir(), "settings.json")
	proxy, gw := "http://127.0.0.1:18431", "https://gw.example.com"
	_, err := SetClaudeGatewayProxy(file, proxy, "csk_test_key", "'/bin/codesynapse' gateway proxy --ensure", time.Now())
	require.NoError(t, err)
	var wg sync.WaitGroup
	for i := range 20 {
		to := proxy
		if i%2 == 0 {
			to = gw
		}
		wg.Go(func() {
			_, err := PointClaudeGateway(file, proxy, gw, to)
			assert.NoError(t, err)
		})
	}
	wg.Wait()
	b, err := os.ReadFile(file) //nolint:gosec // test file
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(b, &doc), "never half a file")
	base, key, ok := ClaudeGatewayConfig(file)
	assert.True(t, ok)
	assert.Equal(t, key, "csk_test_key")
	assert.True(t, base == proxy || base == gw)
}
