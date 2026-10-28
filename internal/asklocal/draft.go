package asklocal

import (
	"errors"
	"fmt"
	"go/parser"
	"go/scanner"
	"go/token"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/astrona-io/codesynapse/internal/gitx"
)

// local_draft: the agent hands a well-specified coding task and some context
// files to a local model, which writes a draft. The draft is data for the
// agent to check against the real file before it applies anything with its
// own edit tool; nothing here writes files.
//
// The model is asked for a replacement code block, not a unified diff: small
// local models miscount hunk headers and misquote context lines often enough
// that their diffs rarely apply, while a block that replaces a named line
// range (or is new code) is easy to check and to apply with one edit. A
// model that answers with a diff anyway gets it checked line by line
// against what was sent (CheckDiff).

// DraftTool is the MCP tool name.
const DraftTool = "local_draft"

// Draft formats.
const (
	FormatCode = "code"
	FormatDiff = "diff"
)

// DraftWarning heads every draft, so no reader mistakes it for checked code.
const DraftWarning = "Unverified draft from a local model. Compare it with the real file and fix it before you apply it with your own edit; never apply it blindly."

// Limits of a draft call, beside the file limits shared with ask_local.
const (
	MaxTask = 4000
	// MaxDraft bounds the code that goes back into the agent's context.
	MaxDraft = 16000
	// MaxNotes bounds the notes beside it.
	MaxNotes = 2000
	// maxProblems bounds the problems listed for one draft.
	maxProblems = 10
)

// DraftIn is the tool's input.
type DraftIn struct {
	Task   string   `json:"task" jsonschema:"what to write, specific and self-contained: the function, test table or boilerplate, its inputs and outputs, and the conventions to follow"`
	Paths  []string `json:"paths" jsonschema:"repository-relative context files or directories (only files git tracks are read); add :start-end for a line range, e.g. internal/api/api.go:100-240"`
	Target string   `json:"target,omitempty" jsonschema:"optional repository-relative file the draft is for; add :start-end when the draft replaces those lines (they are sent to the model too)"`
}

// DraftRequest is what the bridge sends the API.
type DraftRequest struct {
	Task   string `json:"task"`
	Target string `json:"target,omitempty"`
	Files  []File `json:"files"`
}

// Draft is the tool's result.
type Draft struct {
	Warning string `json:"warning"`
	// Format is FormatCode (a replacement or new code) or FormatDiff.
	Format   string `json:"format"`
	Language string `json:"language,omitempty"`
	Code     string `json:"code"`
	Target   string `json:"target,omitempty"`
	// Replaces is the target range the code replaces, when one was sent.
	Replaces *Citation `json:"replaces,omitempty"`
	Notes    string    `json:"notes,omitempty"`
	// Citations in the notes, checked against the lines sent.
	Citations  []Citation `json:"citations"`
	Unverified int        `json:"unverified_citations,omitempty"`
	// Problems found before the agent saw the draft: syntax errors, diff
	// lines that do not match the files, paths outside what was sent.
	Problems    []string `json:"problems,omitempty"`
	Model       string   `json:"model"`
	Files       int      `json:"files"`
	Lines       int      `json:"lines"`
	InputTokens int      `json:"input_tokens"`
}

// Validate checks a draft request against the limits.
func (r DraftRequest) Validate() error {
	t := strings.TrimSpace(r.Task)
	switch {
	case t == "":
		return fmt.Errorf("%w: task is empty", ErrInvalid)
	case utf8.RuneCountInString(t) > MaxTask:
		return fmt.Errorf("%w: task is longer than %d characters", ErrInvalid, MaxTask)
	}
	if r.Target != "" {
		p, _, _, err := ParseSpec(r.Target)
		if err != nil {
			return err
		}
		if !relPath(p) {
			return fmt.Errorf("%w: target %q must be a plain repository-relative path", ErrInvalid, r.Target)
		}
	}
	return Request{Question: r.Task, Files: r.Files}.validateFiles()
}

// relPath reports whether p is a clean repository-relative path.
func relPath(p string) bool {
	return p != "" && !strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "-") && !strings.ContainsRune(p, 0) &&
		!strings.Contains(p, "\\") && path.Clean(p) == p && p != "." && p != ".." && !strings.HasPrefix(p, "../") && !strings.Contains(p, "/../")
}

var (
	fenceRe   = regexp.MustCompile("(?s)```([A-Za-z0-9_+.#-]*)[^\\n]*\\n(.*?)\\n?```")
	numberRe  = regexp.MustCompile(`^\s*\d+\| ?`)
	refusalRe = regexp.MustCompile(`(?i)^\s*CANNOT DRAFT\b:?`)
)

// ErrNoDraft means the model wrote no code: it declined, or answered in prose.
var ErrNoDraft = errors.New("the local model wrote no draft")

// ParseDraft splits a model answer into the first fenced code block and the
// notes around it. A refusal ("CANNOT DRAFT: why") or an answer without a
// code block returns ErrNoDraft with the model's reason as the notes.
// Line-number prefixes copied from the numbered input are removed.
func ParseDraft(answer string) (lang, code, notes string, err error) {
	if refusalRe.MatchString(answer) {
		return "", "", strings.TrimSpace(refusalRe.ReplaceAllString(answer, "")), ErrNoDraft
	}
	loc := fenceRe.FindStringSubmatchIndex(answer)
	if loc == nil {
		return "", "", strings.TrimSpace(answer), ErrNoDraft
	}
	lang = strings.ToLower(answer[loc[2]:loc[3]])
	code = answer[loc[4]:loc[5]]
	notes = strings.TrimSpace(answer[:loc[0]] + "\n" + answer[loc[1]:])
	notes = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(notes), "NOTES:"))
	code = unnumber(code)
	if strings.TrimSpace(code) == "" {
		return lang, "", notes, ErrNoDraft
	}
	return lang, code, notes, nil
}

// unnumber drops "12| " prefixes when every non-empty line carries one: the
// model copied the numbering of its input.
func unnumber(code string) string {
	lines := strings.Split(code, "\n")
	seen := false
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if !numberRe.MatchString(l) {
			return code
		}
		seen = true
	}
	if !seen {
		return code
	}
	for i, l := range lines {
		lines[i] = numberRe.ReplaceAllString(l, "")
	}
	return strings.Join(lines, "\n")
}

// IsDiff reports whether a draft is a unified diff rather than code.
func IsDiff(lang, code string) bool {
	if lang == "diff" || lang == "patch" {
		return true
	}
	c := strings.TrimLeft(code, "\n")
	return strings.HasPrefix(c, "diff --git ") || strings.HasPrefix(c, "--- ") && strings.Contains(c, "\n+++ ") && strings.Contains(c, "\n@@ ")
}

var hunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// CheckDiff lists what keeps diff from applying to the files sent: paths
// that are not repository-relative or not among the files sent or target,
// and hunk lines (context and removals) that are not the sent lines at
// those numbers. A new file may only be target. Nil means every hunk
// matches what the model was shown.
func CheckDiff(diff string, files []File, target string) []string {
	var problems []string
	add := func(format string, args ...any) {
		if len(problems) < maxProblems {
			problems = append(problems, fmt.Sprintf(format, args...))
		}
	}
	changes, err := gitx.ParseDiff([]byte(diff))
	if err != nil {
		return []string{"the diff does not parse: " + err.Error()}
	}
	if len(changes) == 0 {
		return []string{"the diff changes no file"}
	}
	known := map[string]bool{}
	for _, f := range files {
		known[f.Path] = true
	}
	for _, c := range changes {
		for _, p := range []string{c.Path, c.OldPath} {
			switch {
			case p == "":
			case !relPath(p):
				add("%s is not a plain repository-relative path", p)
			case !known[p] && p != target:
				add("%s was neither sent nor the target", p)
			}
		}
	}

	// Walk the hunks: every context and removed line must be the sent line
	// at that number of the old file.
	lineAt := func(p string, n int) (string, bool) {
		for _, f := range files {
			if f.Path != p || n < f.Start || n > f.End {
				continue
			}
			i := f.Start
			for l := range strings.Lines(f.Text) {
				if i == n {
					return strings.TrimRight(l, "\r\n"), true
				}
				i++
			}
		}
		return "", false
	}
	cur, oldN, oldLeft, newLeft := "", 0, 0, 0
	for _, l := range strings.Split(strings.TrimSuffix(diff, "\n"), "\n") { // the newline ends the last line; it starts no blank one
		if oldLeft > 0 || newLeft > 0 {
			if l == "" { // a blank context line whose space was trimmed
				l = " "
			}
			switch l[0] {
			case ' ', '-':
				if oldLeft == 0 {
					add("%s: hunk has more old lines than its header says", cur)
					oldLeft, newLeft = 0, 0
					continue
				}
				if want, ok := lineAt(cur, oldN); !ok {
					add("%s:%d was not among the lines sent", cur, oldN)
				} else if want != strings.TrimRight(l[1:], "\r") {
					add("%s:%d does not match the file", cur, oldN)
				}
				oldN++
				oldLeft--
				if l[0] == ' ' {
					newLeft--
				}
				continue
			case '+':
				newLeft--
				continue
			case '\\': // "\ No newline at end of file"
				continue
			}
			add("%s: hunk ends before its header says", cur)
			oldLeft, newLeft = 0, 0
		}
		switch {
		case strings.HasPrefix(l, "--- "):
			cur = diffSide(l[4:])
		case strings.HasPrefix(l, "+++ "):
			if p := diffSide(l[4:]); p != "" {
				cur = p
			}
		case strings.HasPrefix(l, "@@ "):
			m := hunkRe.FindStringSubmatch(l)
			if m == nil {
				add("%s: bad hunk header %q", cur, clip(l, 60))
				continue
			}
			oldN, _ = strconv.Atoi(m[1])
			oldLeft, newLeft = count(m[2]), count(m[4])
		}
	}
	if oldLeft > 0 || newLeft > 0 {
		add("%s: the last hunk is shorter than its header says", cur)
	}
	return problems
}

// diffSide is the path of a ---/+++ line, "" for /dev/null.
func diffSide(s string) string {
	s, _, _ = strings.Cut(s, "\t")
	s = strings.TrimSpace(s)
	if s == "/dev/null" {
		return ""
	}
	if strings.HasPrefix(s, "a/") || strings.HasPrefix(s, "b/") {
		return s[2:]
	}
	return s
}

func count(s string) int {
	if s == "" {
		return 1
	}
	n, _ := strconv.Atoi(s)
	return n
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// CheckGo parses Go code as a file, as declarations or as statements (a
// draft is often a function or a test table, not a whole file) and returns
// the syntax errors of the closest fit, with line numbers of the draft.
func CheckGo(code string) []string {
	try := func(prefix, suffix string) []string {
		fset := token.NewFileSet()
		_, err := parser.ParseFile(fset, "draft.go", prefix+code+suffix, parser.SkipObjectResolution)
		if err == nil {
			return nil
		}
		shift := strings.Count(prefix, "\n")
		var out []string
		var list scanner.ErrorList
		if errors.As(err, &list) {
			for _, e := range list {
				if len(out) == maxProblems {
					break
				}
				out = append(out, fmt.Sprintf("go syntax: line %d: %s", max(1, e.Pos.Line-shift), e.Msg))
			}
			return out
		}
		return []string{"go syntax: " + err.Error()}
	}
	first := try("", "")
	if first == nil {
		return nil
	}
	if strings.HasPrefix(strings.TrimSpace(code), "package ") {
		return first
	}
	decl := try("package draft\n", "")
	if decl == nil {
		return nil
	}
	if try("package draft\nfunc _() {\n", "\n}") == nil {
		return nil
	}
	return decl
}

// IsGo reports whether a draft for target in lang is Go code.
func IsGo(lang, target string) bool {
	if lang == "go" || lang == "golang" {
		return true
	}
	p, _, _, _ := ParseSpec(target)
	return lang == "" && strings.HasSuffix(p, ".go")
}
