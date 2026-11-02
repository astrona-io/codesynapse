package accounts

import (
	"encoding/json"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestOldRoleNames keeps the names used before the rename working in
// requests for one release.
func TestOldRoleNames(t *testing.T) {
	for in, want := range map[string]Role{
		"none": Deny, "viewer": Read, "editor": Write, "member": Write, "admin": Full,
		"deny": Deny, "guest": Guest, "read": Read, "write": Write, "full": Full, "owner": Owner,
		"bogus": "bogus",
	} {
		t.Run(in, func(t *testing.T) {
			assert.Equal(t, ParseRole(in), want)
			var got struct {
				Role  Role            `json:"role"`
				Roles map[string]Role `json:"roles"`
			}
			require.NoError(t, json.Unmarshal([]byte(`{"role":"`+in+`","roles":{"g":"`+in+`"}}`), &got))
			assert.Equal(t, got.Role, want)
			assert.Equal(t, got.Roles["g"], want)
		})
	}
	var r Role
	assert.Error(t, json.Unmarshal([]byte(`1`), &r))
	out, err := json.Marshal(struct{ Role Role }{Write})
	require.NoError(t, err)
	assert.Equal(t, string(out), `{"Role":"write"}`)

	for in, want := range map[string]string{"member": TeamRead, "maintainer": TeamFull, "read": TeamRead, "full": TeamFull, "": ""} {
		assert.Equal(t, ParseTeamRelation(in), want, in)
	}
}
