package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/usage"
)

func TestBashClass(t *testing.T) {
	tests := []struct{ cmd, want string }{
		{"git status", usage.BashGitStatus},
		{"git -C /x --no-pager diff --stat", usage.BashGitDiff},
		{"cd app && git log --oneline -5", usage.BashGitLog},
		{"git blame -L 1,5 main.go", usage.BashGitBlame},
		{"gh pr checks 276", usage.BashGh},
		{"gh pr view --json state", usage.BashGh},
		{"gh run view 42 --log-failed", usage.BashGh},
		{"gh run list --branch x", usage.BashGh},
		{"gh run rerun 42", ""},
		{"gh pr merge 276 --squash", ""},
		{"gh pr create --title x", ""},
		{"gh api repos/x/y", ""},
		{"FOO=1 grep -rn Foo .", usage.BashSearch},
		{"rg Foo | head", usage.BashSearch},
		{"cat internal/x/y.go", usage.BashRead},
		{"tail -n 200 build.log", usage.BashRead},
		{"head -n 20 README.md", usage.BashRead},
		{"sed -n '10,40p' internal/x/y.go", usage.BashRead},
		{"sed -n 10,40p README.md", usage.BashRead},
		{"sed -i 's/a/b/' internal/x/y.go", ""},
		{"sed -i.bak -n 1p y.go", ""},
		{"sed -n 10,40p", ""},
		{"git log | sed -n 1,5p", usage.BashGitLog},
		{"cat /dev/null", ""},
		{"go test ./...", usage.BashTest},
		{"go test ./internal/gittools", ""},
		{"npm test", usage.BashTest},
		{"git commit -m 'git status'", ""},
		{"echo hi", ""},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) { assert.Equal(t, bashClass(tt.cmd), tt.want) })
	}
}

// File reads in the shell are pointed at ask_local first: a log or a long
// file is exactly what a local model can read in the agent's place.
func TestBashReadHintNamesAskLocal(t *testing.T) {
	assert.True(t, strings.HasPrefix(bashHints[usage.BashRead], "ask_local"))
}

func TestBashHintOncePerClass(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	in := toolHookInput{SessionID: "s-1", ToolName: "Bash"}
	in.ToolInput.Command = "git status"
	ctx := context.Background()
	assert.True(t, bashHint(ctx, in) != nil, "first git status hints")
	assert.True(t, bashHint(ctx, in) == nil, "second does not")
	s := loadState(stateFile("s-1"))
	assert.Equal(t, s.Bash[usage.BashGitStatus], 2)
	assert.Equal(t, s.bashReport()[usage.BashGitStatus], 2)
	s.BashSent = map[string]int{usage.BashGitStatus: 2}
	assert.True(t, s.bashReport() == nil, "nothing new after a report")
}

func TestReadTarget(t *testing.T) {
	tests := []struct {
		cmd, file  string
		start, end int
	}{
		{"sed -n '10,40p' internal/x/y.go", "internal/x/y.go", 10, 40},
		{"sed -n 7p y.go", "y.go", 7, 7},
		{"sed -n -e 10,40p y.go", "y.go", 10, 40},
		{"sed -n '40,10p' y.go", "y.go", 0, 0},
		{"sed -n '/func/,/^}/p' y.go", "y.go", 0, 0},
		{"head -n 20 README.md", "README.md", 1, 20},
		{"head -30 a.go", "a.go", 1, 30},
		{"tail -n 50 build.log", "build.log", 0, 0},
		{"cd x && cat -n a.go", "a.go", 0, 0},
		{"cat $HOME/a.go", "", 0, 0},
		{"cat a*.go", "", 0, 0},
		{"git status", "", 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			file, start, end := readTarget(tt.cmd)
			assert.Equal(t, file, tt.file)
			assert.Equal(t, start, tt.start)
			assert.Equal(t, end, tt.end)
		})
	}
}

func contextOf(out map[string]any) string {
	spec, _ := out["hookSpecificOutput"].(map[string]any)
	s, _ := spec["additionalContext"].(string)
	return s
}

// Reads and searches are steered every time, with the replacement for that
// very command; the other classes still hint once.
func TestBashSteersEveryRead(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	ctx := context.Background()
	in := toolHookInput{SessionID: "s-steer", ToolName: "Bash"}
	for range 3 {
		in.ToolInput.Command = "sed -n '10,40p' internal/x/y.go"
		assert.Contains(t, contextOf(bashHint(ctx, in)), `read_ranges reads "internal/x/y.go:10-40"`)
		in.ToolInput.Command = "rg -n Foo internal"
		assert.Contains(t, contextOf(bashHint(ctx, in)), "Instead of grep")
	}
	s := loadState(stateFile("s-steer"))
	assert.Equal(t, s.Bash[usage.BashRead], 3)
	assert.Equal(t, s.Bash[usage.BashSearch], 3)

	t.Setenv("CODESYNAPSE_BASH_GUARD", "false")
	in.SessionID = "s-off"
	in.ToolInput.Command = "rg Foo"
	assert.Equal(t, contextOf(bashHint(ctx, in)), "CodeSynapse: "+bashHints[usage.BashSearch])
	assert.True(t, bashHint(ctx, in) == nil, "switched off: once per class, as before")
}

// Case: a whole cat of a large tracked source file is the shell twin of a
// whole Read and is turned back once; everything the hook is not sure of
// runs as asked.
func TestBashTurnsBackWholeCat(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	ctx := context.Background()
	dir := gitRepo(t)
	big := strings.Repeat("// x\n", 500)
	for _, f := range []string{"big.go", "other.go", "notes.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, f), []byte("package demo\n"+big), 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "small.go"), []byte("package demo\n"), 0o600))
	gitIn(t, dir, "add", "big.go", "other.go", "notes.txt", "small.go")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "new.go"), []byte("package demo\n"+big), 0o600))

	run := func(session, cmd string) map[string]any {
		in := toolHookInput{SessionID: session, Cwd: dir, ToolName: "Bash"}
		in.ToolInput.Command = cmd
		return bashHint(ctx, in)
	}
	denied := func(out map[string]any) bool {
		spec, _ := out["hookSpecificOutput"].(map[string]any)
		return spec["permissionDecision"] == "deny"
	}
	out := run("c-1", "cat big.go")
	require.True(t, denied(out), "a whole cat of a large tracked source file is turned back")
	reason := out["hookSpecificOutput"].(map[string]any)["permissionDecisionReason"].(string)
	assert.Contains(t, reason, "run the command again")
	assert.Contains(t, reason, "ask_local")
	assert.False(t, denied(run("c-1", "cat big.go")), "running it again is allowed")
	assert.True(t, denied(run("c-1", "cat -n "+filepath.Join(dir, "other.go"))), "absolute path with -n")
	assert.Equal(t, loadState(stateFile("c-1")).Turned, 2)

	for _, cmd := range []string{
		"cat small.go",
		"cat new.go",
		"cat notes.txt",
		"cat big.go | head -50",
		"cat big.go > /tmp/copy.go",
		"cat big.go other.go",
		"cat -A big.go",
		"cat 'big.go'",
		"cat $F",
		"cd x && cat big.go",
		"sed -n 1,500p big.go",
		"bat big.go",
	} {
		t.Run(cmd, func(t *testing.T) { assert.False(t, denied(run("c-2", cmd)), "runs as asked") })
	}

	t.Setenv("CODESYNAPSE_READ_GUARD_LINES", "0")
	assert.False(t, denied(run("c-3", "cat big.go")), "the read guard's 0 turns it off too")
	t.Setenv("CODESYNAPSE_READ_GUARD_LINES", "")
	t.Setenv("CODESYNAPSE_BASH_GUARD", "false")
	assert.False(t, denied(run("c-4", "cat big.go")), "switched off")
	t.Setenv("CODESYNAPSE_BASH_GUARD", "")

	data, err := json.Marshal(offlineMarker{At: time.Now().UTC()})
	require.NoError(t, err)
	writePrivate(offlineFile("c-5"), data)
	out = run("c-5", "cat big.go")
	assert.False(t, denied(out), "never while CodeSynapse is offline")
	assert.Equal(t, contextOf(out), "CodeSynapse: "+bashHints[usage.BashRead], "offline: the plain once-per-class hint")
}

func FuzzBashHintParse(f *testing.F) {
	for _, s := range []string{"sed -n '10,40p' a.go", "head -n 5 a.go", "cat -n a.go | rg x", "git log", "head -", "sed -n 0p"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, cmd string) {
		file, start, end := readTarget(cmd)
		if start > 0 && (file == "" || end < start) {
			t.Fatalf("range %d-%d for %q", start, end, file)
		}
		_ = bashClass(cmd)
		_ = bashSteer(usage.BashRead, cmd)
	})
}
