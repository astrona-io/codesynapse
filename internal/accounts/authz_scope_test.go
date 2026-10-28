package accounts

import (
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

// A graph two organisations share is personal only when it is someone's own
// in both, whichever entry comes first.
func TestScopePersonalAcrossOrgs(t *testing.T) {
	for _, tc := range []struct {
		name         string
		first, other bool
		want         bool
	}{
		{"both personal", true, true, true},
		{"personal first", true, false, false},
		{"organisation first", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := Access{Orgs: map[string]Role{"a": Member, "b": Member}, Repos: []RepoAccess{
				{OrgID: "a", Graph: "g", Role: Viewer, Personal: tc.first},
				{OrgID: "b", Graph: "g", Role: Viewer, Personal: tc.other},
			}}
			got := a.Scope(map[string]bool{"a": true, "b": true})["g"]
			assert.True(t, got.Ambiguous)
			assert.Equal(t, got.Personal, tc.want)
		})
	}
}
