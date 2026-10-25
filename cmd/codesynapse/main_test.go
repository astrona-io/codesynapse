package main

import (
	"os"
	"testing"
)

// Tests must never report hook activity to a developer's running stack with
// the token in their config folder; the test that needs it turns it back on.
func TestMain(m *testing.M) {
	_ = os.Setenv("CODESYNAPSE_HOOK_ACTIVITY", "false") //nolint:errcheck // test setup
	os.Exit(m.Run())
}
