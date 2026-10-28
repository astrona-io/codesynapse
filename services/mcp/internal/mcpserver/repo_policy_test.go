package mcpserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/ingest"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/security"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestMCPSecurityUsesTenantPolicy(t *testing.T) {
	ctx := context.Background()
	url, keys, acc, _, reader := keyServer(t)
	policy, err := repoconfig.Parse([]byte("version: 1\nscanners:\n  security:\n    ignore_rules:\n      - rule_id: yaml-hardcoded-secret\n        paths: [mock.yaml]\n"))
	require.NoError(t, err)
	canonical, err := json.Marshal(policy.Config())
	require.NoError(t, err)
	graph := storetest.Fixture()
	repo, _ := graph.Node(storetest.ID(model.KindRepository, storetest.Repo))
	metadata, err := json.Marshal(map[string]any{"repository_policy": ingest.PolicyMetadata{Present: true, Digest: policy.Digest(), Config: policy.Config()}})
	require.NoError(t, err)
	repo.Meta = string(metadata)
	graph.AddNode(model.Node{ID: storetest.ID(model.KindFinding, "mock"), Repo: storetest.Repo, Kind: model.KindFinding, Name: "Literal secret", File: "mock.yaml", Rule: "yaml-hardcoded-secret", Severity: "high"})
	require.NoError(t, reader.ReplaceRepo(ctx, storetest.Repo, graph))
	principal, err := acc.AuthenticateKey(ctx, keys["a"])
	require.NoError(t, err)
	principal.Via = "session"
	require.NoError(t, acc.SetRepoPolicy(ctx, principal, accounts.RepoPolicyApproval{OrgID: principal.OrgID, Graph: storetest.Repo, Digest: policy.Digest(), ConfigJSON: string(canonical), Reason: "Approved mock fixture", AllowSuppressions: true}))
	a, err := connectKey(t, url, keys["a"])
	require.NoError(t, err)
	b, err := connectKey(t, url, keys["b"], "chat-b")
	require.NoError(t, err)
	var approved, other security.Report
	require.False(t, call(t, a, "security_report", map[string]any{}, &approved).IsError)
	require.False(t, call(t, b, "security_report", map[string]any{}, &other).IsError)
	require.Len(t, approved.Suppressed, 1)
	assert.Equal(t, approved.FindingCounts["high"], 0)
	assert.Len(t, other.Suppressed, 0)
	assert.Equal(t, other.FindingCounts["high"], 1)
	require.NoError(t, acc.DeleteRepoPolicy(ctx, principal, principal.OrgID, storetest.Repo))
	var revoked security.Report
	require.False(t, call(t, a, "security_report", map[string]any{}, &revoked).IsError)
	assert.Len(t, revoked.Suppressed, 0)
	assert.Equal(t, revoked.FindingCounts["high"], 1)

	// Triage applies to the deciding organisation only, without a policy.
	finding, err := reader.Node(ctx, storetest.ID(model.KindFinding, "mock"))
	require.NoError(t, err)
	key, err := security.FindingKey(ctx, reader, finding)
	require.NoError(t, err)
	_, err = acc.SetFindingTriage(ctx, principal, accounts.FindingTriage{OrgID: principal.OrgID, Graph: storetest.Repo, Fingerprint: key, Rule: finding.Rule, File: finding.File, State: accounts.TriageFalsePositive, Reason: "Mock credential for tests"})
	require.NoError(t, err)
	var triaged, untriaged security.Report
	require.False(t, call(t, a, "security_report", map[string]any{}, &triaged).IsError)
	require.False(t, call(t, b, "security_report", map[string]any{}, &untriaged).IsError)
	require.Len(t, triaged.Suppressed, 1)
	require.True(t, triaged.Suppressed[0].Triage != nil)
	assert.Equal(t, triaged.Suppressed[0].Triage.State, accounts.TriageFalsePositive)
	assert.Equal(t, triaged.FindingCounts["high"], 0)
	assert.Len(t, untriaged.Suppressed, 0)
	assert.Equal(t, untriaged.FindingCounts["high"], 1)

	filterFindingsByPath(&approved, "different/")
	assert.Len(t, approved.Suppressed, 0)
	assert.Equal(t, approved.SuppressedCounts["high"], 0)
}
