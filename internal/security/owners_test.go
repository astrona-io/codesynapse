package security

import (
	"bytes"
	"context"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestApplyOwners(t *testing.T) {
	rules := []model.OwnerRule{
		{Pattern: "*", Owners: []string{"@acme/platform"}},
		{Pattern: "/web/", Owners: []string{"@acme/web", "@lead"}},
		{Pattern: "/vendor/", Owners: []string{}},
	}
	rep := Report{
		Findings: []Finding{
			{Rule: "a", Severity: "low", Location: "internal/x.go:3"},
			{Rule: "b", Severity: "high", Location: "web/app.ts:9-12"},
			{Rule: "c", Severity: "medium", Location: "web/page.vue"},
			{Rule: "d", Severity: "medium", Location: "vendor/lib.go:1"},
		},
		Suppressed: []Finding{{Rule: "e", Severity: "high", Location: "web/old.ts:1", SuppressionReason: "accepted"}},
	}
	rep.applyOwners(rules)

	assert.Equal(t, rep.Findings[0].CodeOwners, []string{"@acme/platform"})
	assert.Equal(t, rep.Findings[1].CodeOwners, []string{"@acme/web", "@lead"})
	assert.Len(t, rep.Findings[3].CodeOwners, 0)
	assert.Equal(t, rep.Suppressed[0].CodeOwners, []string{"@acme/web", "@lead"})

	var got []string
	for _, o := range rep.OwnerCounts {
		got = append(got, o.Owner)
	}
	assert.Equal(t, got, []string{"@acme/web", "@lead", "", "@acme/platform"}, "the high finding first, then by medium; suppressed findings are not counted")
	assert.Equal(t, rep.OwnerCounts[0].Findings, 2)

	var md bytes.Buffer
	require.NoError(t, WriteMarkdown(&md, rep))
	assert.Contains(t, md.String(), "## Findings by owner (CODEOWNERS)")
	assert.Contains(t, md.String(), "| no owner | 1 |")

	none := Report{Findings: []Finding{{Rule: "a", Severity: "low", Location: "x.go:1"}}}
	none.applyOwners(nil)
	assert.Len(t, none.OwnerCounts, 0)
	assert.Len(t, none.Findings[0].CodeOwners, 0)
}

func TestDeprecatedDependencies(t *testing.T) {
	ctx := context.Background()
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID("r", model.KindRepository, "r"), Kind: model.KindRepository, Repo: "r", Name: "r"})
	mod := func(name string, indirect bool, deprecated string, behind int) {
		g.AddNode(model.Node{ID: model.NodeID("r", model.KindModule, name), Kind: model.KindModule, Repo: "r", Name: name, Version: "1.0.0",
			Indirect: indirect, Deprecated: deprecated, Latest: "2.0.0", Behind: behind})
	}
	mod("zz-direct", false, "moved to zz2", 3)
	mod("aa-indirect", true, "unmaintained", 1)
	mod("fine", false, "", 0)
	g.AddNode(model.Node{ID: model.NodeID("r", model.KindModule, "example.com/bad"), Kind: model.KindModule, Repo: "r", Name: "example.com/bad", Version: "v1.2.0",
		Ecosystem: "Go", Retracted: "published with a broken | build"})
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, "r", g))
	rep, err := Build(ctx, st, "r", Options{})
	require.NoError(t, err)
	assert.Equal(t, rep.Modules.Deprecated, 2)
	assert.Equal(t, rep.Modules.Behind, 2)
	require.Len(t, rep.Deprecated, 2)
	assert.Equal(t, rep.Deprecated[0].Module, "zz-direct", "direct dependencies first")
	var md bytes.Buffer
	require.NoError(t, WriteMarkdown(&md, rep))
	assert.Contains(t, md.String(), "| `zz-direct` | 1.0.0 | 2.0.0 | yes | moved to zz2 |")
	assert.Equal(t, rep.Modules.Retracted, 1)
	require.Len(t, rep.Retracted, 1)
	assert.Equal(t, rep.Retracted[0].Reason, "published with a broken | build")
	assert.Contains(t, md.String(), "## Retracted Go module versions")
	assert.Contains(t, md.String(), "| `example.com/bad` | v1.2.0 | ? | yes | published with a broken \\| build |")
}
