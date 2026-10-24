package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestReadGuard(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	dir := t.TempDir()
	var src strings.Builder
	src.WriteString("package big\n\ntype Store struct{}\n\n")
	for i := range 200 {
		src.WriteString("// F does a thing.\nfunc (s *Store) F" + string(rune('A'+i%26)) + string(rune('a'+i/26)) + "() {\n}\n\n")
	}
	big := filepath.Join(dir, "big.go")
	require.NoError(t, os.WriteFile(big, []byte(src.String()), 0o600))
	small := filepath.Join(dir, "small.go")
	require.NoError(t, os.WriteFile(small, []byte("package big\n"), 0o600))

	read := func(file string, offset int) map[string]any {
		var in toolHookInput
		in.SessionID, in.ToolInput.FilePath, in.ToolInput.Offset = "chat-1", file, offset
		return readGuard(in)
	}
	out := read(big, 0)
	require.True(t, out != nil, "the first whole read of a large file is turned back")
	spec := out["hookSpecificOutput"].(map[string]any)
	assert.Equal(t, spec["permissionDecision"], "deny")
	reason := spec["permissionDecisionReason"].(string)
	assert.Contains(t, reason, "type Store: lines 3-3")
	assert.Contains(t, reason, "func (*Store).FAa: lines 6-7")
	assert.Contains(t, reason, "read it again")
	assert.True(t, read(big, 0) == nil, "reading it again is allowed")
	assert.True(t, read(big, 100) == nil, "a ranged read is never turned back")
	assert.True(t, read(small, 0) == nil, "small files are read as asked")

	t.Setenv("CODESYNAPSE_READ_GUARD_LINES", "0")
	var in toolHookInput
	in.SessionID, in.ToolInput.FilePath = "chat-2", big
	assert.True(t, readGuard(in) == nil, "0 turns the guard off")
}

func TestStopGuard(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	ctx := context.Background()
	dir := gitRepo(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	in := toolHookInput{SessionID: "chat-1", Cwd: dir}
	assert.True(t, stopGuard(ctx, in, logger) == nil, "a clean tree finishes")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "demo.go"), []byte("package demo\n\nfunc Changed() {}\n"), 0o600))
	out := stopGuard(ctx, in, logger)
	require.True(t, out != nil)
	assert.Equal(t, out["decision"], "block")
	assert.Contains(t, out["reason"].(string), "verify_plan")
	assert.True(t, stopGuard(ctx, in, logger) == nil, "asked once per state of the tree")
	in.StopHookActive = true
	require.NoError(t, os.WriteFile(filepath.Join(dir, "demo.go"), []byte("package demo\n\nfunc ChangedAgain() {}\n"), 0o600))
	assert.True(t, stopGuard(ctx, in, logger) == nil, "never while already continuing because of a stop hook")
}

func TestHookCommandPrintsJSON(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	var out bytes.Buffer
	env := &app.Env{Stdin: strings.NewReader(`{"session_id":"chat-9","tool_input":{"file_path":"/nonexistent"}}`), Stdout: &out, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	require.NoError(t, (&hookOptions{EC: env, event: "read"}).Run(context.Background(), nil))
	assert.Equal(t, out.String(), "", "no opinion: no output, exit 0")

	out.Reset()
	env.Stdin = strings.NewReader(`not json`)
	require.NoError(t, (&hookOptions{EC: env, event: "stop"}).Run(context.Background(), nil))
	assert.Equal(t, out.String(), "", "unreadable input: no opinion")
	var none any
	assert.True(t, json.Unmarshal([]byte(`{}`), &none) == nil)
}
