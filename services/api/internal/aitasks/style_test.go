package aitasks

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/prose"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// styledPrompts are the system prompts of the tasks in styled.
var styledPrompts = map[string]string{
	"briefing":         briefingSystem,
	"pr-summary":       prSummarySystem,
	"cleanup-briefing": cleanupBriefingSystem,
}

const testDiff = "diff --git a/app/app.go b/app/app.go\n--- a/app/app.go\n+++ b/app/app.go\n@@ -10,2 +10,3 @@\n line\n+changed\n line\n"

func TestStyledPromptsCarryTheStyleOnce(t *testing.T) {
	assert.Equal(t, len(styledPrompts), len(styled))
	for task, p := range styledPrompts {
		assert.True(t, styled[task], task)
		assert.Equal(t, strings.Count(p, prose.Style), 1, task)
		assert.True(t, strings.HasSuffix(p, prose.Style), task+": the style comes after the task's own format")
	}
	// The JSON tasks keep their structured answers without the style block.
	for _, p := range []string{triageSystem, reviewSystem} {
		assert.Equal(t, strings.Count(p, prose.NoFiller), 0)
	}
}

// Every system prompt reaching the model is a package constant, never text
// formatted per request, so provider prompt caches keep matching.
func TestSystemPromptsAreConstants(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "aitasks.go", nil, 0)
	require.NoError(t, err)
	consts := map[string]bool{}
	for _, d := range f.Decls {
		if g, ok := d.(*ast.GenDecl); ok && g.Tok == token.CONST {
			for _, sp := range g.Specs {
				for _, n := range sp.(*ast.ValueSpec).Names {
					consts[n.Name] = true
				}
			}
		}
	}
	for name := range map[string]bool{"briefingSystem": true, "prSummarySystem": true, "cleanupBriefingSystem": true, "triageSystem": true, "reviewSystem": true} {
		assert.True(t, consts[name], name+" is a constant")
	}
	checked := 0
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name == "ask" || fn.Name.Name == "appendPrompt" {
			continue // these pass their system parameter through
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			var system ast.Expr
			switch n := n.(type) {
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "ask" || sel.Sel.Name == "appendPrompt") && len(n.Args) > 2 {
					system = n.Args[2]
				}
			case *ast.KeyValueExpr:
				if k, ok := n.Key.(*ast.Ident); ok && k.Name == "System" {
					system = n.Value
				}
			}
			if system != nil {
				checked++
				id, ok := system.(*ast.Ident)
				assert.True(t, ok && consts[id.Name], fn.Name.Name+" passes a constant system prompt")
			}
			return true
		})
	}
	assert.True(t, checked >= 10, "the check found the system prompt arguments")
}

// Golden answers are what the styled tasks should produce: they pass the lint,
// and each request carried the task's constant system prompt unchanged.
func TestGoldenOutputsHaveNoFiller(t *testing.T) {
	for task, system := range styledPrompts {
		t.Run(task, func(t *testing.T) {
			golden, err := os.ReadFile(filepath.Join("testdata", task+".md"))
			require.NoError(t, err)
			r, st, _, systems := setupWithSystems(t, string(golden), true)
			id, err := r.Run(taskContext(t), task, Input{Repo: storetest.Repo, Diff: []byte(testDiff)})
			if errors.Is(err, ErrNothingToDo) {
				t.Skip("fixture has no input for this task")
			}
			require.NoError(t, err)
			rv, err := st.GetReview(taskContext(t), id)
			require.NoError(t, err)
			assert.NoError(t, prose.Check(rv.Summary))
			require.True(t, len(*systems) > 0)
			for _, s := range *systems {
				assert.Equal(t, s, system)
			}
		})
	}
}

func TestTestdataHasNoFiller(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.md"))
	require.NoError(t, err)
	require.True(t, len(files) >= len(styledPrompts))
	for _, f := range files {
		b, err := os.ReadFile(f)
		require.NoError(t, err)
		assert.NoError(t, prose.Check(string(b)), f)
	}
}

// The lint measures only: an answer full of filler is stored as it came.
func TestFillerIsMeasuredNotRewritten(t *testing.T) {
	answer := "Great question! Note that the service runs commands. Hope this helps."
	r, st, _ := setup(t, answer, true)
	id, err := r.Run(taskContext(t), "briefing", Input{Repo: storetest.Repo})
	require.NoError(t, err)
	rv, err := st.GetReview(taskContext(t), id)
	require.NoError(t, err)
	assert.Contains(t, rv.Summary, "Note that")
	assert.Error(t, prose.Check(rv.Summary), "the lint flags it")
}
