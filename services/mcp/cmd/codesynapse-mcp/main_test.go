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
