package security

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestApprovedOptionsFailsClosed(t *testing.T) {
	policy, err := repoconfig.Parse([]byte("version: 1\nscanners:\n  security:\n    ignore_rules:\n      - rule_id: yaml-hardcoded-secret\n        paths: [mock.yaml]\n"))
	require.NoError(t, err)
	canonical, err := json.Marshal(policy.Config())
	require.NoError(t, err)
	base := accounts.RepoPolicyApproval{OrgID: "org", Graph: "repo", Digest: policy.Digest(), ConfigJSON: string(canonical), AllowSuppressions: true, Reason: "Approved mock"}
	for _, tc := range []struct {
		name    string
		mutate  func(*accounts.RepoPolicyApproval)
		allowed bool
	}{
		{"active", func(*accounts.RepoPolicyApproval) {}, true},
		{"other tenant", func(a *accounts.RepoPolicyApproval) { a.OrgID = "other" }, false},
		{"other graph", func(a *accounts.RepoPolicyApproval) { a.Graph = "other" }, false},
		{"digest", func(a *accounts.RepoPolicyApproval) { a.Digest = "wrong" }, false},
		{"expired", func(a *accounts.RepoPolicyApproval) { at := time.Now().Add(-time.Minute); a.ExpiresAt = &at }, false},
		{"not approved", func(a *accounts.RepoPolicyApproval) { a.AllowSuppressions = false }, false},
		{"corrupt config", func(a *accounts.RepoPolicyApproval) { a.ConfigJSON = "invalid" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			approval := base
			tc.mutate(&approval)
			opts := ApprovedOptions("low", "org", "repo", policy.Digest(), approval)
			if !tc.allowed {
				assert.True(t, opts.Suppression == nil)
				return
			}
			require.True(t, opts.Suppression != nil)
			assert.Equal(t, opts.Suppression(model.Node{Repo: "repo", Rule: "yaml-hardcoded-secret", File: "mock.yaml"}), "Approved mock")
			assert.Equal(t, opts.Suppression(model.Node{Repo: "foreign", Rule: "yaml-hardcoded-secret", File: "mock.yaml"}), "")
		})
	}
}

func TestApprovedOptionsInlineIgnores(t *testing.T) {
	node := model.Node{Repo: "repo", Rule: "exec-variable", File: "tools/gen.go", Ignore: "fixed build input"}
	for _, tc := range []struct {
		name, config, want string
	}{
		{"approved", "version: 1\nscanners:\n  security:\n    inline_ignores: true\n", "In code: fixed build input"},
		{"not requested", "version: 1\nscanners:\n  security:\n    ignore_rules:\n      - rule_id: yaml-hardcoded-secret\n        paths: [mock.yaml]\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := repoconfig.Parse([]byte(tc.config))
			require.NoError(t, err)
			canonical, err := json.Marshal(policy.Config())
			require.NoError(t, err)
			approval := accounts.RepoPolicyApproval{OrgID: "org", Graph: "repo", Digest: policy.Digest(), ConfigJSON: string(canonical), AllowSuppressions: true, Reason: "Approved"}
			opts := ApprovedOptions("low", "org", "repo", policy.Digest(), approval)
			require.True(t, opts.Suppression != nil)
			assert.Equal(t, opts.Suppression(node), tc.want)
			unmarked := node
			unmarked.Ignore = ""
			assert.Equal(t, opts.Suppression(unmarked), "")
		})
	}
}
