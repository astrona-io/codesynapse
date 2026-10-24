package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestEditObservations(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	dir := t.TempDir()
	file := filepath.Join(dir, "large.go")
	require.NoError(t, os.WriteFile(file, []byte(strings.Repeat("x", 1000)), 0o600))
	in := toolHookInput{SessionID: "edit-session", Cwd: dir, ToolName: "Edit"}
	in.ToolInput.FilePath, in.ToolInput.OldString, in.ToolInput.NewString = file, "old", "new"
	noteEdit(in, false)
	e := loadState(stateFile(in.SessionID)).Edits
	assert.Equal(t, e.Edits, 1)
	assert.Equal(t, e.PatchBytes, 6, "old and new text both cost output")
	assert.Equal(t, e.AvoidedBytes, 994)
	noteEdit(in, true)
	e = loadState(stateFile(in.SessionID)).Edits
	assert.Equal(t, e.Failed, 1)
	assert.Equal(t, e.AvoidedBytes, 994, "failed edits earn no savings")
	in.ToolInput.FilePath = "missing-file"
	noteEdit(in, false)
	e = loadState(stateFile(in.SessionID)).Edits
	assert.Equal(t, e.AvoidedBytes, 994, "unknown baselines earn no savings")
	in.ToolName, in.ToolInput.Content = "Write", strings.Repeat("a", 300)
	noteEdit(in, false)
	e = loadState(stateFile(in.SessionID)).Edits
	assert.Equal(t, e.Writes, 1)
	assert.Equal(t, e.WriteBytes, 300)
	assert.Equal(t, e.AvoidedBytes, 994)
}
