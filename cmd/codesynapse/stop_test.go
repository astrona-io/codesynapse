package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// Cases from real sessions where the Stop hook asked about work that was not
// the session's, or asked in a way the tools answer for the wrong tree
// (docs/PLAN.md, item 18).

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	_, err := gitx.Run(context.Background(), dir, append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	require.NoError(t, err, strings.Join(args, " "))
}

// write writes a file the way the session's agent (agentID "") or a subagent
// does, with the PostToolUse hook seeing it.
func write(t *testing.T, in toolHookInput, agentID, file, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o755))
	require.NoError(t, os.WriteFile(file, []byte(body), 0o600))
	in.ToolName, in.AgentID = "Write", agentID
	in.ToolInput.FilePath, in.ToolInput.Content = file, body
	noteEdit(in, false)
}

// A tracked file someone had changed before the session began is not asked
// about, and neither is its formatting; only what the session changed is.
func TestStopGuardIgnoresAChangeFromBeforeTheSession(t *testing.T) {
	refusingAPI(t)
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := gitRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "demo.go"), []byte("package demo\n\nfunc   Theirs() {}\n"), 0o600))
	in := toolHookInput{SessionID: "chat-dirty", Cwd: dir}
	noteBaseline(ctx, in, logger)
	assert.True(t, stopGuard(ctx, in, logger) == nil, "the person's edit from before the session is not the session's change")

	write(t, in, "", filepath.Join(dir, "mine.go"), "package demo\n\nfunc Mine() {}\n")
	out := stopGuard(ctx, in, logger)
	require.True(t, out != nil, "the session's own change is asked about")
	reason := out["reason"].(string)
	assert.Contains(t, reason, "1 file(s) in acme/demo")
	assert.True(t, !strings.Contains(reason, "demo.go"), "the earlier edit's formatting is not the session's problem: "+reason)
}

// Compaction starts the session's context again, not its work: what it
// changed before is still asked about.
func TestStopGuardAsksAfterCompaction(t *testing.T) {
	refusingAPI(t)
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := gitRepo(t)
	in := toolHookInput{SessionID: "chat-compact", Cwd: dir}
	noteBaseline(ctx, in, logger)
	write(t, in, "", filepath.Join(dir, "demo.go"), "package demo\n\nfunc Changed() {}\n")
	in.Source = "compact"
	noteBaseline(ctx, in, logger)
	assert.True(t, stopGuard(ctx, toolHookInput{SessionID: "chat-compact", Cwd: dir}, logger) != nil)
}

// Case: the session started in the main checkout, which the person keeps
// editing, and the agent works in a worktree of its own. The start checkout
// is not the session's to check; the worktree is, and since the bridge
// answers calls with no arguments for the start checkout, its files are
// named.
func TestStopGuardFollowsTheSessionIntoAnotherWorktree(t *testing.T) {
	refusingAPI(t)
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := gitRepo(t)
	in := toolHookInput{SessionID: "chat-wt", Cwd: dir}
	noteBaseline(ctx, in, logger)
	wt := filepath.Join(t.TempDir(), "scratch-wt")
	gitIn(t, dir, "worktree", "add", "-q", "-b", "feature", wt)

	// The person edits the start checkout while the agent works elsewhere.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "demo.go"), []byte("package demo\n\nfunc Theirs() {}\n"), 0o600))
	write(t, in, "", filepath.Join(wt, "feature.go"), "package demo\n\nfunc Feature() {}\n")

	out := stopGuard(ctx, in, logger)
	require.True(t, out != nil, "the session's change in its worktree is asked about")
	reason := out["reason"].(string)
	assert.Contains(t, reason, "1 file(s) of acme/demo changed in the worktree")
	assert.Contains(t, reason, "scratch-wt")
	assert.Contains(t, reason, `verify_plan with files ["feature.go"]`)
	assert.True(t, !strings.Contains(reason, "no arguments: your current change"), "no-argument calls would check the start checkout: "+reason)
	assert.True(t, !strings.Contains(reason, "in acme/demo changed since"), "the person's edit in the start checkout is not asked about: "+reason)
	assert.True(t, stopGuard(ctx, in, logger) == nil, "asked once per state")

	// A clean start checkout and a committed, pushed worktree: nothing to say.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "demo.go"), []byte("package demo\n\n// Synced is in the server's graph.\nfunc Synced() int { return 1 }\n"), 0o600))
	gitIn(t, wt, "add", "-A")
	gitIn(t, wt, "commit", "-q", "-m", "feature")
	gitIn(t, wt, "branch", "-q", "--set-upstream-to=main")
	gitIn(t, dir, "merge", "-q", "--ff-only", "feature")
	assert.True(t, stopGuard(ctx, in, logger) == nil, "nothing left unchecked: "+reasonOf(stopGuard(ctx, in, logger)))
}

// Case: the session's agent works in another repository altogether. The
// tools cannot answer for it by naming files of this one, and the start
// checkout's changes are the person's: nothing to ask.
func TestStopGuardIgnoresTheStartCheckoutWhenTheSessionWorksInAnotherRepository(t *testing.T) {
	refusingAPI(t)
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := gitRepo(t)
	in := toolHookInput{SessionID: "chat-other", Cwd: dir}
	noteBaseline(ctx, in, logger)
	other := gitRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "demo.go"), []byte("package demo\n\nfunc Theirs() {}\n"), 0o600))
	write(t, in, "", filepath.Join(other, "elsewhere.go"), "package demo\n\nfunc Elsewhere() {}\n")
	assert.True(t, stopGuard(ctx, in, logger) == nil)

	// A scratch file outside any checkout changes nothing about the start
	// checkout: the session can still have changed it (with a shell command).
	in2 := toolHookInput{SessionID: "chat-scratch", Cwd: dir}
	noteBaseline(ctx, in2, logger)
	write(t, in2, "", filepath.Join(t.TempDir(), "notes.go"), "package notes\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sed.go"), []byte("package demo\n\nfunc Sed() {}\n"), 0o600))
	assert.True(t, stopGuard(ctx, in2, logger) != nil)
}

// Case: subagents worked in their own worktrees (.claude/worktrees/agent-*).
// Their changes are theirs: the parent is not asked about them, whether the
// hook input says agent_id or not. A subagent working in the parent's own
// checkout made changes there the parent is asked about, with no arguments.
func TestStopGuardLeavesSubagentWorktreesAlone(t *testing.T) {
	refusingAPI(t)
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := gitRepo(t)
	in := toolHookInput{SessionID: "chat-sub", Cwd: dir}
	noteBaseline(ctx, in, logger)
	agentTree := filepath.Join(dir, ".claude", "worktrees", "agent-a1b2")
	require.NoError(t, os.MkdirAll(filepath.Dir(agentTree), 0o755))
	gitIn(t, dir, "worktree", "add", "-q", "-b", "worktree-agent-a1b2", agentTree)

	write(t, in, "a1b2", filepath.Join(agentTree, "sub.go"), "package demo\n\nfunc Sub() {}\n")
	assert.True(t, stopGuard(ctx, in, logger) == nil, "a subagent's worktree is not the parent's change")
	write(t, in, "", filepath.Join(agentTree, "sub2.go"), "package demo\n\nfunc Sub2() {}\n")
	assert.True(t, stopGuard(ctx, in, logger) == nil, "nor when the hook input has no agent_id")

	write(t, in, "c3d4", filepath.Join(dir, "shared.go"), "package demo\n\nfunc Shared() {}\n")
	out := stopGuard(ctx, in, logger)
	require.True(t, out != nil, "delegated work in the parent's own checkout is the parent's to check")
	assert.Contains(t, out["reason"].(string), "verify_plan (no arguments")

	sub := in
	sub.AgentID = "c3d4"
	assert.True(t, stopGuard(ctx, sub, logger) == nil, "never for a subagent's own stop")
}

// Case: the read guard turned back reads of files nothing it suggested could
// answer for (a saved tool output, a file outside any checkout, an untracked
// one): ask_local reads only files git tracks, and the graph has only those.
func TestReadGuardLeavesUntrackedFilesAlone(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	big := strings.Repeat("line\n", 500)
	read := func(file string) map[string]any {
		var in toolHookInput
		in.SessionID, in.ToolInput.FilePath = "chat-read", file
		return readGuard(context.Background(), in)
	}
	saved := filepath.Join(t.TempDir(), "tool-results", "b1.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(saved), 0o755))
	require.NoError(t, os.WriteFile(saved, []byte(big), 0o600))
	assert.True(t, read(saved) == nil, "a saved tool output outside any checkout")

	dir := gitRepo(t)
	untracked := filepath.Join(dir, "new.go")
	require.NoError(t, os.WriteFile(untracked, []byte("package demo\n"+strings.Repeat("// x\n", 500)), 0o600))
	assert.True(t, read(untracked) == nil, "an untracked file")
	gitIn(t, dir, "add", "new.go")
	assert.True(t, read(untracked) != nil, "the same file once git tracks it")
}

func reasonOf(out map[string]any) string {
	r, _ := out["reason"].(string)
	return r
}
