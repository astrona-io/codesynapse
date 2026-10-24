package main

import (
	"context"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestStdioAccountsRequirePersonalCredential(t *testing.T) {
	for _, tc := range []struct {
		name, database, key string
		wantError           bool
	}{
		{"local", "", "", false},
		{"accounts without credential", "postgres://accounts", "", true},
		{"accounts with credential", "postgres://accounts", "csk_personal", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvDatabaseURL, tc.database)
			t.Setenv("CODESYNAPSE_API_TOKEN", tc.key)
			err := (&serveOptions{transport: "stdio"}).Validate(context.Background(), nil)
			if tc.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// --offline swaps the graph store, not the identity checks: without accounts
// every tool call would be refused, so the server says so at start-up.
func TestOfflineNeedsAccounts(t *testing.T) {
	t.Setenv(EnvDatabaseURL, "")
	err := (&serveOptions{transport: "stdio", offline: "."}).Validate(context.Background(), nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "just up")

	t.Setenv(EnvDatabaseURL, "postgres://accounts")
	t.Setenv("CODESYNAPSE_API_TOKEN", "csk_personal")
	assert.NoError(t, (&serveOptions{transport: "stdio", offline: "."}).Validate(context.Background(), nil))
}
