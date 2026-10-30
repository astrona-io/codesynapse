package security

import (
	"bytes"
	"context"
	"strings"
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

func TestLowScorecard(t *testing.T) {
	ctx := context.Background()
	build := func(scored bool) Report {
		g := model.NewGraph()
		g.AddNode(model.Node{ID: model.NodeID("r", model.KindRepository, "r"), Kind: model.KindRepository, Repo: "r", Name: "r"})
		mod := func(name string, indirect bool, score float64, date string, checks ...string) {
			n := model.Node{ID: model.NodeID("r", model.KindModule, name), Kind: model.KindModule, Repo: "r", Name: name, Version: "1.0.0", Indirect: indirect}
			if scored {
				n.Scorecard, n.ScorecardDate, n.ScorecardChecks = score, date, checks
			}
			g.AddNode(n)
		}
		mod("weak-indirect", true, 2.1, "2026-10-19", "Maintained:0")
		mod("zero", false, 0, "2026-10-18", "Code-Review:0", "Fuzzing:0", "SAST:0")
		mod("weak-direct", false, 2.1, "2026-10-19")
		mod("strong", false, 8.2, "2026-10-19")
		mod("unscored", false, 0, "")
		st := memory.New()
		require.NoError(t, st.ReplaceRepo(ctx, "r", g))
		rep, err := Build(ctx, st, "r", Options{})
		require.NoError(t, err)
		return rep
	}
	rep := build(true)
	assert.Equal(t, rep.Modules.LowScorecard, 3)
	var got []string
	for _, m := range rep.LowScorecard {
		got = append(got, m.Module)
	}
	assert.Equal(t, got, []string{"zero", "weak-direct", "weak-indirect"}, "lowest first, then direct first; a 0 with a date counts, no date does not")
	assert.Len(t, rep.Vulnerabilities, 0, "never a vulnerability")
	assert.Equal(t, rep.Grade, build(false).Grade, "the grade does not change")

	var md bytes.Buffer
	require.NoError(t, WriteMarkdown(&md, rep))
	assert.Contains(t, md.String(), "## Low OpenSSF Scorecard")
	assert.Contains(t, md.String(), "A hygiene note, not a vulnerability")
	assert.Contains(t, md.String(), "| `zero` | 1.0.0 | 0.0 | yes | Code-Review:0, Fuzzing:0, SAST:0 | 2026-10-18 |")
	assert.Contains(t, md.String(), "| `weak-direct` | 1.0.0 | 2.1 | yes | – | 2026-10-19 |")
	assert.False(t, strings.Contains(md.String(), "`strong`"), "a good score is not listed")
}
