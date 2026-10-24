package security

import (
	"bytes"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
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
