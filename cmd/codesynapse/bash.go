package main

import (
	"path"
	"slices"
	"strings"

	"github.com/astrona-io/codesynapse/internal/usage"
)

// hook bash (PreToolUse on Bash) spots shell commands CodeSynapse answers
// with less output: git status, diff, log and blame, text search, reading a
// file with cat/head/tail, and running a whole test suite. It never blocks:
// the first time a session runs a class it adds a hint naming the tool, and
// every occurrence is counted (usage.BashClasses) so Statistics shows how
// often agents reach for the shell where the graph or a git tool would do.
// Only the class is kept, never the command.

// bashHints is what the agent is told, per class.
var bashHints = map[string]string{
	usage.BashGitStatus: "git_status gives branch, upstream and changed files in a few lines.",
	usage.BashGitDiff:   "git_diff_summary lists changed files with line ranges and no patch; read only those ranges. get_pr_context adds symbols, risk and covering tests.",
	usage.BashGitLog:    "git_log lists commits compactly; file_history covers indexed history of a file.",
	usage.BashGitBlame:  "git_blame groups a line range by commit instead of printing every line.",
	usage.BashGh:        "gh_pr_status (wait_seconds blocks until the checks finish or one fails, so do not poll) gives a pull request's state, mergeability, review decision and counted checks (only the failing and waiting ones named); gh_pr_list lists pull requests compactly; gh_run_failure gives the failed jobs and error lines of a CI run instead of its log. Merging and creating stay with gh.",
	usage.BashSearch:    "search finds symbols and files by name; minimal_context finds code by meaning; get_symbol_context lists callers without grepping.",
	usage.BashRead:      "file_outline lists a file's symbols with line ranges; Read with offset and limit takes only the lines you need.",
	usage.BashTest:      "verify_plan lists the closest tests for your change; run those instead of the whole suite.",
}

// bashHint answers a Bash PreToolUse: it counts the command's class and, the
// first time per session, returns the hint. Anything else is no opinion.
func bashHint(in toolHookInput) map[string]any {
	class := bashClass(in.ToolInput.Command)
	state := stateFile(in.SessionID)
	if class == "" || state == "" {
		return nil
	}
	s := loadState(state)
	if s.Bash == nil {
		s.Bash = map[string]int{}
	}
	s.Bash[class]++
	first := !slices.Contains(s.Hinted, class)
	if first {
		s.Hinted = append(s.Hinted, class)
	}
	saveState(state, s)
	if !first {
		return nil
	}
	return map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PreToolUse", "additionalContext": "CodeSynapse: " + bashHints[class],
	}}
}

// bashClass names the CodeSynapse-answerable kind of a shell command, or ""
// for anything else. Only the commands of a simple line are looked at
// (split on &&, ;, | and newlines); the first that matches decides.
func bashClass(command string) string {
	if len(command) > 4096 {
		return ""
	}
	for _, part := range splitShell(command) {
		if c := classOfWords(strings.Fields(part)); c != "" {
			return c
		}
	}
	return ""
}

func splitShell(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == '&' || r == '|' || r == '\n' })
}

func classOfWords(w []string) string {
	// Leading VAR=value assignments and wrappers do not change the command.
	for len(w) > 0 && (strings.Contains(w[0], "=") && !strings.HasPrefix(w[0], "-") || w[0] == "sudo" || w[0] == "time") {
		w = w[1:]
	}
	if len(w) == 0 {
		return ""
	}
	cmd := path.Base(w[0])
	args := w[1:]
	switch cmd {
	case "git":
		// Skip global options such as -C dir and --no-pager.
		for len(args) > 0 && strings.HasPrefix(args[0], "-") {
			if args[0] == "-C" || args[0] == "-c" {
				args = args[min(2, len(args)):]
				continue
			}
			args = args[1:]
		}
		if len(args) == 0 {
			return ""
		}
		switch args[0] {
		case "status":
			return usage.BashGitStatus
		case "diff":
			return usage.BashGitDiff
		case "log":
			return usage.BashGitLog
		case "blame":
			return usage.BashGitBlame
		case "grep":
			return usage.BashSearch
		}
	case "gh":
		// Only the reads the gh tools answer; create, merge, run and api are
		// the developer's.
		if len(args) >= 2 && (args[0] == "pr" && slices.Contains([]string{"view", "checks", "status", "list"}, args[1]) ||
			args[0] == "run" && slices.Contains([]string{"view", "list"}, args[1])) {
			return usage.BashGh
		}
	case "grep", "egrep", "fgrep", "rg", "ag", "ack":
		return usage.BashSearch
	case "cat", "head", "tail", "bat", "less":
		for _, a := range args {
			if !strings.HasPrefix(a, "-") && strings.Contains(path.Base(a), ".") {
				return usage.BashRead
			}
		}
	case "go":
		if len(args) > 0 && args[0] == "test" && slices.Contains(args, "./...") {
			return usage.BashTest
		}
	case "pytest", "jest", "vitest":
		if len(args) == 0 {
			return usage.BashTest
		}
	case "npm", "pnpm", "yarn":
		if len(args) > 0 && (args[0] == "test" || len(args) > 1 && args[0] == "run" && args[1] == "test") && len(args) <= 2 {
			return usage.BashTest
		}
	case "cargo":
		if len(args) == 1 && args[0] == "test" {
			return usage.BashTest
		}
	}
	return ""
}

// bashReport is the per-class count since the last report; nil when none.
func (s hookState) bashReport() map[string]int {
	var out map[string]int
	for class, n := range s.Bash {
		if d := n - s.BashSent[class]; d > 0 {
			if out == nil {
				out = map[string]int{}
			}
			out[class] = d
		}
	}
	return out
}
