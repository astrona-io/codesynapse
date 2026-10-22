package security

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestSuppressionIsTenantPresentation(t *testing.T) {
	ctx := context.Background()
	reader := fixture(t)
	other, err := Build(ctx, reader, storetest.Repo, Options{})
	require.NoError(t, err)
	approved, err := Build(ctx, reader, storetest.Repo, Options{Suppression: func(n model.Node) string {
		if n.Rule == "exec-shell" {
			return "Approved test harness exception"
		}
		return ""
	}})
	require.NoError(t, err)
	require.Len(t, approved.Suppressed, 1)
	assert.Equal(t, approved.Suppressed[0].SuppressionReason, "Approved test harness exception")
	assert.Equal(t, approved.SuppressedCounts["critical"], 1)
	assert.Equal(t, approved.FindingCounts["critical"], 0)
	assert.Equal(t, approved.Unanalysed, 0)
	assert.Greater(t, approved.Score, other.Score)
	assert.Equal(t, other.FindingCounts["critical"], 1)
	assert.Len(t, other.Suppressed, 0)
	expired, err := Build(ctx, reader, storetest.Repo, Options{Suppression: func(model.Node) string { return "" }})
	require.NoError(t, err)
	assert.Equal(t, expired, other)
	raw, err := reader.FindingCounts(ctx, storetest.Repo)
	require.NoError(t, err)
	assert.Equal(t, raw["critical"], 1)
	var md strings.Builder
	require.NoError(t, WriteMarkdown(&md, approved))
	assert.Contains(t, md.String(), "Approved suppressions")
	assert.Contains(t, md.String(), "Approved test harness exception")
}

func TestSuppressionLeavesUnanalysedFindingsActive(t *testing.T) {
	ctx := context.Background()
	graph := model.NewGraph()
	graph.AddNode(model.Node{ID: model.NodeID("r", model.KindRepository, "r"), Repo: "r", Kind: model.KindRepository, Name: "r"})
	for i := 0; i < store.MaxLimit+3; i++ {
		name := fmt.Sprintf("finding-%04d", i)
		graph.AddNode(model.Node{ID: model.NodeID("r", model.KindFinding, name), Repo: "r", Kind: model.KindFinding, Name: name, Severity: "critical", Rule: "exec-shell"})
	}
	reader := memory.New()
	require.NoError(t, reader.ReplaceRepo(ctx, "r", graph))
	report, err := Build(ctx, reader, "r", Options{Suppression: func(model.Node) string { return "Approved exception" }})
	require.NoError(t, err)
	assert.Equal(t, report.SuppressedCounts["critical"], store.MaxLimit)
	assert.Equal(t, report.FindingCounts["critical"], 3)
	assert.Equal(t, report.Unanalysed, 3)
	assert.Equal(t, report.Score, 25)
	assert.True(t, report.ExceedsThreshold("critical"))
}
