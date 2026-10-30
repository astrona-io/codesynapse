package main

import (
	"os"
	"testing"
)

// Tests must never report hook activity to a developer's running stack with
// the token in their config folder; the test that needs it turns it back on.
func TestMain(m *testing.M) {
	_ = os.Setenv("CODESYNAPSE_HOOK_ACTIVITY", "false") //nolint:errcheck // test setup
	// Offline markers and kept tool lists never land in the developer's cache.
	cache, err := os.MkdirTemp("", "codesynapse-test-cache")
	if err != nil {
		panic(err)
	}
	bridgeCacheRoot = func() (string, error) { return cache, nil }
	code := m.Run()
	_ = os.RemoveAll(cache) //nolint:errcheck // test cleanup
	os.Exit(code)
}
