package agentrules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
		return p
	}
	text, custom := Load("")
	assert.Equal(t, text, Default)
	assert.True(t, !custom)

	text, custom = Load(write("mine.txt", "  Use CodeSynapse first.\n"))
	assert.Equal(t, text, "Use CodeSynapse first.")
	assert.True(t, custom)

	for name, p := range map[string]string{
		"missing": filepath.Join(dir, "none.txt"),
		"empty":   write("empty.txt", " \n"),
		"huge":    write("huge.txt", strings.Repeat("x", MaxBytes+1)),
	} {
		text, custom = Load(p)
		assert.Equal(t, text, Default, name)
		assert.True(t, !custom, name)
	}
}

func TestDefaultNamesRealTools(t *testing.T) {
	for _, tool := range []string{"minimal_context", "get_symbol_context", "get_implementations", "get_call_graph", "file_outline", "change_checklist", "verify_plan", "get_pr_context", "search", "duplicate_code", "ask_local"} {
		assert.Contains(t, Default, tool)
	}
	assert.True(t, len(Default) < MaxBytes, "it is added to every session")
}

// The answer-style block keeps negations and exact text, and names the cases
// that stay in full sentences.
func TestDefaultAnswerStyle(t *testing.T) {
	for _, want := range []string{
		"Answer first", "Never drop a negation", "not, never, no, only, except",
		"numbers, code, commands, paths and error text exact", "changed lines plus 1-2 lines of context",
		"security warnings", "irreversible actions", "ordered steps", "questions or options",
		"code, comments, commits, docs, PRs",
	} {
		assert.Contains(t, Default, want)
	}
}
