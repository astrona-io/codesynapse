package main

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/astrona-io/codesynapse/internal/usage"
)

// hook bash (PreToolUse on Bash) spots shell commands CodeSynapse answers
// with less output: git status, diff, log and blame, text search, reading a
// file with cat/head/tail or a sed -n range, and running a whole test suite.
// The first time a session runs a class it adds a hint naming the tool, and
// every occurrence is counted (usage.BashClasses) so Statistics shows how
// often agents reach for the shell where the graph or a git tool would do.
// Only the class is kept, never the command.
//
// Reading code is what fills a cloud agent's context over a long session, so
// file reads and text searches are steered on every occurrence, with the
// exact replacement (read_ranges with the file:start-end the command named).
// A plain "cat file" of a large source file git tracks is turned back the
// first time, exactly like a whole Read (readGuard): the person's command is
// never blocked twice, and anything the hook is not sure of (pipes,
// redirects, globs, variables, several commands, untracked or small files,
// CodeSynapse offline) runs as asked. CODESYNAPSE_BASH_GUARD=false turns both
// off and leaves the once-per-class hints.

// bashHints is what the agent is told, per class.
var bashHints = map[string]string{
	usage.BashGitStatus: "git_status gives branch, upstream and changed files in a few lines.",
	usage.BashGitDiff:   "git_diff_summary lists changed files with line ranges and no patch; read only those ranges. get_pr_context adds symbols, risk and covering tests.",
	usage.BashGitLog:    "git_log lists commits compactly; file_history covers indexed history of a file.",
	usage.BashGitBlame:  "git_blame groups a line range by commit instead of printing every line.",
	usage.BashGh:        "gh_pr_status (wait_seconds blocks until the checks finish or one fails, so do not poll) gives a pull request's state, mergeability, review decision and counted checks (only the failing and waiting ones named); gh_pr_list lists pull requests compactly; gh_run_list lists recent CI runs (by workflow, branch or conclusion); gh_run_failure gives the failed jobs and error lines of a CI run instead of its log. Merging and creating stay with gh.",
	usage.BashSearch:    "search finds symbols and files by name; minimal_context finds code by meaning; get_symbol_context lists callers without grepping.",
	usage.BashRead:      "ask_local has a local model read files or logs and answer your question with file:line citations, so they stay out of your context; file_outline lists a file's symbols with line ranges; Read with offset and limit takes only the lines you need.",
	usage.BashTest:      "verify_plan lists the closest tests for your change; run those instead of the whole suite.",
}

// bashHint answers a Bash PreToolUse: it counts the command's class, turns
// back a whole cat of a large tracked source file, steers reads and searches
// every time and hints the other classes the first time per session. Anything
// else is no opinion.
func bashHint(ctx context.Context, in toolHookInput) map[string]any {
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
	guard := bashGuardOn() && (class == usage.BashRead || class == usage.BashSearch) && !codesynapseOffline(in.SessionID)
	if guard && class == usage.BashRead {
		if file, lines := catTurnBack(ctx, in, s.Read); file != "" {
			s.Read = append(s.Read, file)
			s.Turned++
			saveState(state, s)
			return turnBack(file, lines, "To read the whole file anyway, run the command again.")
		}
	}
	saveState(state, s)
	var text string
	switch {
	case guard:
		text = bashSteer(class, in.ToolInput.Command)
	case first:
		text = bashHints[class]
	default:
		return nil
	}
	return map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PreToolUse", "additionalContext": "CodeSynapse: " + text,
	}}
}

// bashGuardOn is the switch for steering every read and search and for
// turning back whole cat reads; on unless CODESYNAPSE_BASH_GUARD is false.
func bashGuardOn() bool {
	on, err := strconv.ParseBool(os.Getenv("CODESYNAPSE_BASH_GUARD"))
	return err != nil || on
}

// sedRange is a sed -n print script of one line range: 10,40p or 10p.
var sedRange = regexp.MustCompile(`^(\d{1,7})(?:,(\d{1,7}))?p$`)

// bashSteer is the short hint for one read or search: the replacement for
// that command, with the file and lines it named when they are plain.
func bashSteer(class, command string) string {
	if class == usage.BashSearch {
		return "Instead of grep: search finds symbols and files by name, minimal_context finds code by meaning, get_symbol_context lists callers, and ask_local answers where something is handled with file:line citations."
	}
	file, start, end := readTarget(command)
	switch {
	case file != "" && start > 0:
		return "Instead of the shell, read_ranges reads \"" + file + ":" + strconv.Itoa(start) + "-" + strconv.Itoa(end) + "\" (several ranges in one call)."
	case file != "":
		return "Instead of reading " + file + " in the shell: ask_local answers questions about it with file:line citations, file_outline lists its symbols with line ranges, and read_ranges reads only the \"" + file + ":start-end\" ranges you need."
	}
	return "Instead of reading files in the shell, read_ranges reads file:start-end ranges and ask_local answers questions about files."
}

// readTarget is the file and line range of the first file read in a command:
// sed -n 'A,Bp' file, head -n N file, or a whole read (start 0). The file is
// "" when the read is not one plain path.
func readTarget(command string) (file string, start, end int) {
	for _, part := range splitShell(command) {
		w := commandWords(strings.Fields(part))
		if len(w) == 0 || classOfWords(w) != usage.BashRead {
			continue
		}
		args := w[1:]
		var words []string
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				words = append(words, strings.Trim(a, `'"`))
			}
		}
		if len(words) == 0 {
			return "", 0, 0
		}
		file = words[len(words)-1]
		if !plainPath(file) {
			return "", 0, 0
		}
		switch path.Base(w[0]) {
		case "sed":
			if len(words) == 2 {
				if m := sedRange.FindStringSubmatch(words[0]); m != nil {
					start, _ = strconv.Atoi(m[1]) //nolint:errcheck // digits only
					end = start
					if m[2] != "" {
						end, _ = strconv.Atoi(m[2]) //nolint:errcheck // digits only
					}
					if start > 0 && end >= start {
						return file, start, end
					}
				}
			}
		case "head":
			if n := headLines(args); n > 0 {
				return file, 1, n
			}
		}
		return file, 0, 0
	}
	return "", 0, 0
}

// headLines is the N of head -n N, head -nN or head -N; 0 when not given.
func headLines(args []string) int {
	for i, a := range args {
		var v string
		switch {
		case a == "-n" && i+1 < len(args):
			v = args[i+1]
		case strings.HasPrefix(a, "-n"):
			v = a[2:]
		case strings.HasPrefix(a, "-") && len(a) > 1:
			v = a[1:]
		}
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 1e7 {
			return n
		}
	}
	return 0
}

// plainPath is a path with nothing the shell would expand or reinterpret.
func plainPath(p string) bool {
	return p != "" && len(p) <= 1024 && !strings.ContainsAny(p, "$`*?[]{}~<>()'\"\\ \t!")
}

// catTurnBack names the file of a command that is only "cat [-n] FILE" when
// that file is a large source file git tracks and has not been turned back
// in this session (turned): the shell twin of readGuard. Anything else, and
// every doubt, is "".
func catTurnBack(ctx context.Context, in toolHookInput, turned []string) (string, int) {
	command := strings.TrimSpace(in.ToolInput.Command)
	limit := readGuardLimit()
	if limit <= 0 || len(command) > 1024 || strings.ContainsAny(command, "|;&<>`$\n(){}*?~'\"\\") {
		return "", 0
	}
	w := strings.Fields(command)
	if len(w) < 2 || w[0] != "cat" {
		return "", 0
	}
	var file string
	for _, a := range w[1:] {
		switch {
		case a == "-n" || a == "--number":
			// Numbered lines are still the whole file.
		case strings.HasPrefix(a, "-") || file != "":
			return "", 0 // other flags or several files: not a plain read
		default:
			file = a
		}
	}
	if file == "" {
		return "", 0
	}
	if !filepath.IsAbs(file) {
		if !filepath.IsAbs(in.Cwd) {
			return "", 0
		}
		file = filepath.Join(in.Cwd, file)
	}
	file = filepath.Clean(file)
	if !indexable(file) || slices.Contains(turned, file) {
		return "", 0
	}
	lines := countLines(file, limit)
	if lines < limit || !guardable(ctx, file) {
		return "", 0
	}
	return file, lines
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

// commandWords drops leading VAR=value assignments and wrappers, which do
// not change the command.
func commandWords(w []string) []string {
	for len(w) > 0 && (strings.Contains(w[0], "=") && !strings.HasPrefix(w[0], "-") || w[0] == "sudo" || w[0] == "time") {
		w = w[1:]
	}
	return w
}

func classOfWords(w []string) string {
	w = commandWords(w)
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
	case "sed":
		// A range read (sed -n '10,40p' file) is a file read; sed -i edits and
		// a sed fed by a pipe has no file, so neither counts.
		var words []string
		for _, a := range args {
			switch {
			case a == "--in-place" || strings.HasPrefix(a, "-i") || strings.HasPrefix(a, "--in-place="):
				return ""
			case !strings.HasPrefix(a, "-"):
				words = append(words, a)
			}
		}
		if (slices.Contains(args, "-n") || slices.Contains(args, "--quiet") || slices.Contains(args, "--silent")) && len(words) >= 2 && strings.Contains(path.Base(words[len(words)-1]), ".") {
			return usage.BashRead
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
