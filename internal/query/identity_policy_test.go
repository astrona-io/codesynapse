package query

import (
	"encoding/json"
	"testing"

	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/identity"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestIdentityPolicyHidesExcludedEvidence(t *testing.T) {
	policy, err := repoconfig.Parse([]byte("version: 1\nignore:\n  paths: [excluded/**]\n"))
	require.NoError(t, err)
	meta, err := json.Marshal(map[string]any{"repository_policy": map[string]any{"present": true, "digest": policy.Digest(), "config": policy.Config()}})
	require.NoError(t, err)
	report := identity.Report{Symbols: []identity.Symbol{
		{ID: "visible", Active: true, Current: identity.Location{File: "current.go"}, History: []identity.Location{{File: "excluded/old.go"}, {File: "earlier.go"}}, Candidates: []string{"excluded", "other"}},
		{ID: "excluded", Active: false, Current: identity.Location{File: "excluded/secret.go"}},
		{ID: "other", Active: false, Current: identity.Location{File: "removed.go"}},
		{ID: "package", Active: false},
	}}
	visible, err := filterIdentityPolicy(report, string(meta))
	require.NoError(t, err)
	require.Len(t, visible.Symbols, 2)
	assert.Equal(t, visible.Symbols[0].History, []identity.Location{{File: "earlier.go"}})
	assert.Equal(t, visible.Symbols[0].HistoryOmitted, 1)
	assert.Equal(t, visible.Symbols[0].Candidates, []string{"other"})
	assert.Equal(t, visible.Symbols[0].CandidatesOmitted, 1)
	assert.Len(t, report.Symbols, 4)
	assert.Len(t, report.Symbols[0].History, 2)
	_, err = filterIdentityPolicy(report, `{"repository_policy":{"present":true,"digest":"wrong","config":{"version":1}}}`)
	assert.Error(t, err)
}
