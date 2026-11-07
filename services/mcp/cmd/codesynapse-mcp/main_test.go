package main

import (
	"context"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestStdioAccountsRequirePersonalCredential(t *testing.T) {
	for _, tc := range []struct {
		name, database, key, api string
		wantError                bool
	}{
		{"local", "", "", "", false},
		{"accounts without credential", "postgres://accounts", "", "http://api:8080", true},
		{"accounts with credential", "postgres://accounts", "csk_personal", "http://api:8080", false},
		// Access is decided by the API; without it the server refuses to start.
		{"accounts without the API", "postgres://accounts", "csk_personal", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvDatabaseURL, tc.database)
			t.Setenv("CODESYNAPSE_API_TOKEN", tc.key)
			err := (&serveOptions{transport: "stdio", apiURL: tc.api}).Validate(context.Background(), nil)
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
	assert.NoError(t, (&serveOptions{transport: "stdio", offline: ".", apiURL: "http://api:8080"}).Validate(context.Background(), nil))
}
