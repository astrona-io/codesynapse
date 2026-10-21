package review

import (
	"context"
	"strings"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"

	"github.com/parisnakitakejser/codesynapse/internal/gitx"
	"github.com/parisnakitakejser/codesynapse/internal/query"
	"github.com/parisnakitakejser/codesynapse/internal/store/memory"
	"github.com/parisnakitakejser/codesynapse/internal/store/storetest"
)

func TestScore(t *testing.T) {
	tests := []struct {
		name string
		in   Signals
		want Level
	}{
		{"trivial tested private", Signals{Tested: true}, Low},
		{"untested private", Signals{}, Low},
		{"exported untested", Signals{Exported: true}, Medium},
		{"exec untested exported", Signals{Exported: true, Tags: []string{"exec"}}, High},
		{"hub with exec and sql", Signals{FanIn: 10, Impacted: 40, Exported: true, Complexity: 20, Tags: []string{"exec", "sql"}}, Critical},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score, reasons := Score(tt.in)
			assert.Equal(t, LevelFor(score), tt.want, "score %d reasons %v", score, reasons)
			assert.LessOrEqual(t, score, 100)
		})
	}
}

func TestAnalyze(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	require.NoError(t, s.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	svc := query.Service{R: s}

	// Lines 26-27 sit inside (*app.impl).Do (25-30).
	rep, err := Analyze(ctx, svc, storetest.Repo, []gitx.FileChange{
		{Path: "app/app.go", Ranges: []gitx.LineRange{{Start: 26, End: 27}}},
		{Path: "web/app.vue", Ranges: []gitx.LineRange{{Start: 1, End: 3}}},
	}, Options{})
	require.NoError(t, err)

	require.Len(t, rep.Changes, 1)
	c := rep.Changes[0]
	assert.Equal(t, c.Name, "(*app.impl).Do")
	assert.Equal(t, c.Tags, []string{"exec"})
	require.Len(t, c.Tests, 1)
	assert.Equal(t, c.Tests[0].Name, "app.TestHandler")

	names := map[string]bool{}
	for _, i := range rep.Impacted {
		names[i.Name] = true
	}
	assert.True(t, names["app.Handler"])
	assert.True(t, names["app.main"])
	assert.Equal(t, rep.Summary.SecuritySensitive, 1)
	assert.Equal(t, rep.Summary.UntestedChanges, 0)
	assert.False(t, rep.Files[1].Indexed)

	var md strings.Builder
	require.NoError(t, WriteMarkdown(&md, rep))
	assert.Contains(t, md.String(), "(*app.impl).Do")
	assert.Contains(t, md.String(), "Command execution")
}
