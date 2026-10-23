package taskeval

import (
	"context"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestParseLog(t *testing.T) {
	out := []byte("\x1eaaa\x1ffeat(x): add a thing\n\nM\tinternal/x/x.go\nA\tinternal/x/new.go\nD\told.go\n" +
		"\x1ebbb\x1fdocs: no files\n")
	commits := parseLog(out)
	require.Len(t, commits, 2)
	assert.Equal(t, commits[0].Hash, "aaa")
	assert.Equal(t, commits[0].Subject, "feat(x): add a thing")
	assert.Equal(t, commits[0].Changes, [][2]string{{"M", "internal/x/x.go"}, {"A", "internal/x/new.go"}, {"D", "old.go"}})
	assert.Len(t, commits[1].Changes, 0)
}

func TestTasks(t *testing.T) {
	indexed := map[string]bool{"a.go": true, "b.go": true, "README.md": true}
	many := Commit{Hash: "many", Subject: "refactor: everything"}
	for range 30 {
		many.Changes = append(many.Changes, [2]string{"M", "a.go"})
	}
	tasks := Tasks([]Commit{
		{Hash: "1", Subject: "feat: change a", Changes: [][2]string{{"M", "a.go"}, {"A", "c.go"}, {"M", "a.go"}}},
		{Hash: "2", Subject: "chore(deps): bump", Changes: [][2]string{{"M", "a.go"}}},
		{Hash: "3", Subject: "Revert \"feat: change a\"", Changes: [][2]string{{"M", "a.go"}}},
		{Hash: "4", Subject: "feat: only new files", Changes: [][2]string{{"A", "d.go"}}},
		{Hash: "5", Subject: "fix: unindexed file", Changes: [][2]string{{"M", "go.sum"}}},
		{Hash: "6", Subject: "docs: readme and b", Changes: [][2]string{{"M", "README.md"}, {"D", "b.go"}}},
		many,
	}, indexed, Options{})
	require.Len(t, tasks, 2)
	assert.Equal(t, tasks[0], Task{Commit: "1", Subject: "feat: change a", Truth: []string{"a.go"}, Added: 1})
	assert.Equal(t, tasks[1].Truth, []string{"README.md", "b.go"}, "a deleted file existed at base")
}

func TestRecallAndFileOf(t *testing.T) {
	assert.Equal(t, fileOf("internal/a/a.go:10-20"), "internal/a/a.go")
	assert.Equal(t, fileOf("internal/a/a.go:7"), "internal/a/a.go")
	assert.Equal(t, fileOf("docs/a.md"), "docs/a.md")
	assert.Equal(t, fileOf("C:weird/name.go"), "C:weird/name.go")
	truth := []string{"a", "b"}
	files := []string{"x", "a", "y", "z", "w", "b"}
	assert.Equal(t, recall(truth, files, 5), 0.5)
	assert.Equal(t, recall(truth, files, 10), 1.0)
	assert.Equal(t, recall(truth, nil, 10), 0.0)
}

func TestRunScoresBothTools(t *testing.T) {
	ctx := context.Background()
	g := model.NewGraph()
	add := func(name, file string) {
		g.AddNode(model.Node{ID: model.NodeID("r", model.KindFunction, name), Kind: model.KindFunction, Repo: "r", Name: name, File: file, StartLine: 1, EndLine: 5})
	}
	add("ParseInvoice", "billing/invoice.go")
	add("SendReminder", "mail/reminder.go")
	mem := memory.New()
	require.NoError(t, mem.ReplaceRepo(ctx, "r", g))
	assert.Equal(t, IndexedFiles(g), map[string]bool{"billing/invoice.go": true, "mail/reminder.go": true})

	rep := Run(ctx, mem, "r", []Task{{Commit: "1", Subject: "fix ParseInvoice rounding", Truth: []string{"billing/invoice.go"}}})
	require.Len(t, rep.Results, 1)
	for _, tool := range []string{ToolMinimalContext, ToolSearch} {
		s := rep.Results[0].Scores[tool]
		assert.Equal(t, s.Error, "", tool)
		assert.Equal(t, s.Recall10, 1.0, tool)
		assert.True(t, s.Tokens > 0, tool)
		assert.Equal(t, rep.Summary[tool].Hit10, 1.0, tool)
	}
}

func TestRegressions(t *testing.T) {
	base := Report{Summary: map[string]Summary{
		ToolMinimalContext: {Recall5: 0.30, Recall10: 0.40, Hit10: 0.60, Tokens: 1000},
		ToolSearch:         {Recall5: 0.20, Recall10: 0.30, Hit10: 0.50, Tokens: 800},
	}}
	same := base
	assert.Len(t, Regressions(same, base, 0.02), 0)
	worse := Report{Summary: map[string]Summary{
		ToolMinimalContext: {Recall5: 0.29, Recall10: 0.30, Hit10: 0.60, Tokens: 1100, Errors: 1},
		ToolSearch:         base.Summary[ToolSearch],
	}}
	got := Regressions(worse, base, 0.02)
	assert.Equal(t, got, []string{
		"minimal_context recall@10 fell from 0.400 to 0.300",
		"minimal_context tokens grew from 1000 to 1100",
		"minimal_context errors grew from 0 to 1",
	})
}
