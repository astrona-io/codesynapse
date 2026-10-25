package main

import (
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
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
		{"gh pr merge 276 --squash", ""},
		{"gh pr create --title x", ""},
		{"gh api repos/x/y", ""},
		{"FOO=1 grep -rn Foo .", usage.BashSearch},
		{"rg Foo | head", usage.BashSearch},
		{"cat internal/x/y.go", usage.BashRead},
		{"head -n 20 README.md", usage.BashRead},
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

func TestBashHintOncePerClass(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	in := toolHookInput{SessionID: "s-1", ToolName: "Bash"}
	in.ToolInput.Command = "git status"
	assert.True(t, bashHint(in) != nil, "first git status hints")
	assert.True(t, bashHint(in) == nil, "second does not")
	s := loadState(stateFile("s-1"))
	assert.Equal(t, s.Bash[usage.BashGitStatus], 2)
	assert.Equal(t, s.bashReport()[usage.BashGitStatus], 2)
	s.BashSent = map[string]int{usage.BashGitStatus: 2}
	assert.True(t, s.bashReport() == nil, "nothing new after a report")
}
