package security

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestTriageSuppressesForTheOrganisation(t *testing.T) {
	ctx := context.Background()
	reader := fixture(t)
	shell, err := reader.Node(ctx, storetest.ID(model.KindFinding, "app/app.go:27:exec-shell"))
	require.NoError(t, err)
	key, err := FindingKey(ctx, reader, shell)
	require.NoError(t, err)
	plain, err := Build(ctx, reader, storetest.Repo, Options{})
	require.NoError(t, err)

	now := time.Now()
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	row := accounts.FindingTriage{Fingerprint: key, Rule: "exec-shell", File: "app/app.go", State: accounts.TriageFixedBy, Reference: "#12", Reason: "Removed the shell call", ActorName: "Ada", CreatedAt: now.Add(-2 * time.Hour)}
	policy := func(n model.Node) string {
		if n.Rule == "exec-shell" {
			return "Approved harness exception"
		}
		return ""
	}
	cases := []struct {
		name       string
		expires    *time.Time
		policy     func(model.Node) string
		suppressed bool
		reason     string
		triaged    bool
	}{
		{name: "active", suppressed: true, reason: "Fixed by #12: Removed the shell call", triaged: true},
		{name: "not yet expired", expires: &future, suppressed: true, reason: "Fixed by #12: Removed the shell call", triaged: true},
		{name: "expired", expires: &past},
		{name: "policy wins", policy: policy, suppressed: true, reason: "Approved harness exception"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := row
			r.ExpiresAt = tc.expires
			rep, err := Build(ctx, reader, storetest.Repo, WithTriage(Options{Suppression: tc.policy}, []accounts.FindingTriage{r}, now))
			require.NoError(t, err)
			if !tc.suppressed {
				assert.Len(t, rep.Suppressed, 0)
				assert.Equal(t, rep.Score, plain.Score)
				return
			}
			require.Len(t, rep.Suppressed, 1)
			got := rep.Suppressed[0]
			assert.Equal(t, got.Fingerprint, key)
			assert.Equal(t, got.SuppressionReason, tc.reason)
			assert.Equal(t, got.Triage != nil, tc.triaged)
			if tc.triaged {
				assert.Equal(t, got.Triage.By, "Ada")
				assert.Equal(t, got.Triage.State, accounts.TriageFixedBy)
			}
			assert.Equal(t, rep.FindingCounts["critical"], 0)
			assert.Greater(t, rep.Score, plain.Score)
			var md strings.Builder
			require.NoError(t, WriteMarkdown(&md, rep))
			assert.Contains(t, md.String(), tc.reason)
		})
	}
	raw, err := reader.FindingCounts(ctx, storetest.Repo)
	require.NoError(t, err)
	assert.Equal(t, raw["critical"], 1, "triage never changes the stored findings")
}

func TestFindingKeySurvivesLineMoves(t *testing.T) {
	ctx := context.Background()
	keyAt := func(line int, owner string) string {
		g := model.NewGraph()
		g.AddNode(model.Node{ID: model.NodeID("r", model.KindRepository, "r"), Repo: "r", Kind: model.KindRepository, Name: "r"})
		f := model.Node{ID: model.NodeID("r", model.KindFinding, "a.go:x"), Repo: "r", Kind: model.KindFinding, Name: "x", Rule: "exec-shell", Severity: "high", File: "a.go", StartLine: line, EndLine: line}
		g.AddNode(f)
		if owner != "" {
			fn := model.Node{ID: model.NodeID("r", model.KindFunction, owner), Repo: "r", Kind: model.KindFunction, Name: owner, QualifiedName: "p." + owner, File: "a.go"}
			g.AddNode(fn)
			g.AddEdge(model.Edge{From: fn.ID, To: f.ID, Kind: model.EdgeHasFinding})
		}
		st := memory.New()
		require.NoError(t, st.ReplaceRepo(ctx, "r", g))
		key, err := FindingKey(ctx, st, f)
		require.NoError(t, err)
		rep, err := Build(ctx, st, "r", Options{})
		require.NoError(t, err)
		require.Len(t, rep.Findings, 1)
		assert.Equal(t, rep.Findings[0].Fingerprint, key, "the report and FindingKey agree")
		return key
	}
	assert.Equal(t, keyAt(10, "Run"), keyAt(40, "Run"))
	assert.Equal(t, keyAt(10, ""), keyAt(40, ""))
	assert.NotEqual(t, keyAt(10, "Run"), keyAt(10, "Stop"))
	assert.NotEqual(t, keyAt(10, "Run"), keyAt(10, ""))
}
