package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/usage"
)

// Claude Code hooks of the CodeSynapse plugin (docs/PLAN.md, agent workflow
// roadmap step 9; plugins/codesynapse). Like the memory hooks they never
// fail a session: any problem means "no opinion" and exit 0.
//
//   - hook read (PreToolUse on Read): the first time in a session the agent
//     reads all of a large file, it is turned back with the file's outline
//     and line ranges, so it reads what it needs. Reading it again is
//     allowed: the guard nudges, it does not forbid.
//   - hook stop (Stop): when the working tree changed since the last check,
//     the agent is asked once to run verify_plan and get_pr_context before
//     it finishes. The tools do the work (through the bridge's cached
//     working-tree graph), so the hook itself stays instant.
//   - hook session (SessionStart): the CodeSynapse ruleset goes into the
//     session's context, from the server (GET /api/v1/agent-rules) so the
//     wording improves everywhere at once, else the built-in text.
//   - hook lookup (PreToolUse on Read, Grep, Glob and the CodeSynapse
//     tools): notes whether the session's first code lookup was a CodeSynapse
//     tool or a native one. Stop reports it with the number of reads the
//     guard turned back, so Statistics shows whether agents use CodeSynapse
//     first. It never blocks.
//   - hook bash (PreToolUse on Bash): hints, once per class and session, at the
//     CodeSynapse tool that answers git status/diff/log/blame, a text search,
//     a file read or a whole test run with less output, and counts each one
//     (bash.go). It never blocks.
//   - hook answer (PostToolUse on the CodeSynapse tools) and hook read-done
//     (PostToolUse on Read) count, per tool, the file line ranges answers
//     gave and how many the agent then read (agent workflow roadmap step
//     13). Stop reports the counts to the API: tool names and numbers only,
//     never paths, ranges or the session.

// readGuardLines is how many lines make a file large enough to guard;
// CODESYNAPSE_READ_GUARD_LINES overrides it and 0 turns the guard off.
const readGuardLines = 400

// maxOutline bounds the outline a turned-back read gets.
const maxOutline = 60

type toolHookInput struct {
	TranscriptPath string `json:"transcript_path"`
	Client         string `json:"-"`
	SessionID      string `json:"session_id"`
	Cwd            string `json:"cwd"`
	StopHookActive bool   `json:"stop_hook_active"`
	ToolName       string `json:"tool_name"`
	ToolInput      struct {
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
		Content   string `json:"content"`
		Patch     string `json:"patch"`
		FilePath  string `json:"file_path"`
		Command   string `json:"command"`
		Offset    int    `json:"offset"`
		Limit     int    `json:"limit"`
		Repo      string `json:"repo"`
	} `json:"tool_input"`
	ToolResponse json.RawMessage `json:"tool_response"`
	Prompt       string          `json:"prompt"` // UserPromptSubmit
}

// hookState is what the hooks remember per chat session.
type hookState struct {
	Edits     usage.EditCounts  `json:"edits,omitempty"`
	TokenSent usage.TokenCounts `json:"token_sent,omitempty"`
	Read      []string          `json:"read,omitempty"` // files already turned back once
	Stop      string            `json:"stop,omitempty"` // the tree the last stop check saw
	// Ranges are the line ranges answers gave, oldest first.
	Ranges []answerRange `json:"ranges,omitempty"`
	// Counts are the per-tool totals so far, Sent what Stop has reported.
	Counts map[string]rangeCounts `json:"counts,omitempty"`
	Sent   map[string]rangeCounts `json:"sent,omitempty"`
	// First is how the session began: its first code lookup was a
	// "codesynapse" tool or a "native" one (Read, Grep, Glob). TurnedBack
	// counts whole-file reads the guard sent back; Reported is what Stop
	// has already told the server about both.
	First    string       `json:"first,omitempty"`
	Turned   int          `json:"turned,omitempty"`
	Reported workflowSent `json:"reported,omitempty"`
	// Base is the checkout as the session began (edits.go).
	Base *treeBase `json:"base,omitempty"`
	// Bash counts the shell commands CodeSynapse answers with less output, per
	// usage class; Hinted are the classes already hinted, BashSent what Stop
	// has reported (bash.go).
	Bash     map[string]int `json:"bash,omitempty"`
	BashSent map[string]int `json:"bash_sent,omitempty"`
	Hinted   []string       `json:"hinted,omitempty"`
	// Registered: a hook already registered the session with the server.
	Registered bool `json:"registered,omitempty"`
	// Recalled are the memory fact IDs already put in this chat's context
	// (autorecall.go), so a prompt never repeats one.
	Recalled []string `json:"recalled,omitempty"`
	// Task is the session's first prompt, kept only with eval capture on
	// (cases.go).
	Task string `json:"task,omitempty"`
}

// workflowSent is what the server already knows of a session's workflow.
type workflowSent struct {
	First  bool `json:"first,omitempty"`
	Turned int  `json:"turned,omitempty"`
	// Edited and Found are the edit counts already reported (edits.go).
	Edited int `json:"edited,omitempty"`
	Found  int `json:"found,omitempty"`
}

// answerRange is one file:start-end an answer gave.
type answerRange struct {
	Tool  string `json:"t"`
	Repo  string `json:"r,omitempty"` // the repository the call named, if any
	File  string `json:"f"`
	Start int    `json:"s"`
	End   int    `json:"e"`
	Read  bool   `json:"d,omitempty"`
}

type rangeCounts struct {
	Returned int `json:"returned"`
	Read     int `json:"read"`
}

// stateFile is per session, in the plugin's data folder (or the user
// cache). A session ID that is not a plain ID gets no state.
func stateFile(session string) string {
	if execution.Validate(execution.Identity{TenantID: "t", SubjectID: "s", SessionID: session, ParentSessionID: session}) != nil {
		return ""
	}
	dir := os.Getenv("CLAUDE_PLUGIN_DATA")
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(cache, "codesynapse", "hooks")
	}
	return filepath.Join(dir, "session-"+session+".json")
}

func loadState(file string) hookState {
	var s hookState
	if raw, err := os.ReadFile(file); err == nil { //nolint:gosec // our own state file
		_ = json.Unmarshal(raw, &s) //nolint:errcheck // a damaged file means a fresh state
	}
	return s
}

func saveState(file string, s hookState) {
	if len(s.Read) > 200 {
		s.Read = s.Read[len(s.Read)-200:]
	}
	if len(s.Ranges) > maxTrackedRanges {
		s.Ranges = s.Ranges[len(s.Ranges)-maxTrackedRanges:]
	}
	raw, err := json.Marshal(s)
	if err != nil || os.MkdirAll(filepath.Dir(file), 0o700) != nil {
		return
	}
	_ = os.WriteFile(file, raw, 0o600) //nolint:errcheck // best effort
}

type hookOptions struct {
	EC    *app.Env
	event string
}

func (o *hookOptions) SetupFlags(context.Context, *app.Env) error { return nil }
func (o *hookOptions) Complete(context.Context, *app.Env) error   { return nil }
func (o *hookOptions) Validate(context.Context, *app.Env) error   { return nil }

func (o *hookOptions) Run(ctx context.Context, _ *app.Env) error {
	var in toolHookInput
	raw, _ := io.ReadAll(io.LimitReader(o.EC.Stdin, 1<<20)) //nolint:errcheck // no input, no opinion
	_ = json.Unmarshal(raw, &in)                            //nolint:errcheck // same
	var out map[string]any
	started := time.Now()
	switch o.event {
	case "read":
		out = readGuard(in)
	case "stop":
		reportRetrieval(ctx, in, o.EC.Logger)
		captureCase(ctx, in, o.EC.Logger)
		out = stopGuard(ctx, in, o.EC.Logger)
	case "prompt":
		notePrompt(in)
	case "lookup":
		noteLookup(in)
	case "bash":
		out = bashHint(in)
	case "session":
		out = sessionRules(ctx, in, o.EC.Logger)
	case "answer":
		noteAnswer(in)
	case "read-done":
		noteRead(in)
	case "edit", "edit-failed":
		noteEdit(in, o.event == "edit-failed")
	}
	note := recordActivity(ctx, o.event, raw, o.EC.Logger)
	if os.Getenv("CODESYNAPSE_HOOK_TRACE") == "chat" {
		// systemMessage is shown to the person in the chat, not to the model.
		if out == nil {
			out = map[string]any{}
		}
		out["systemMessage"] = fmt.Sprintf("CodeSynapse hook %s: %s (%dms)", o.event, note, time.Since(started).Milliseconds())
	}
	if out != nil {
		return json.NewEncoder(o.EC.Stdout).Encode(out)
	}
	return nil
}

// readGuard turns back the first whole read of a large file in a session.
func readGuard(in toolHookInput) map[string]any {
	limit := readGuardLines
	if v, err := strconv.Atoi(os.Getenv("CODESYNAPSE_READ_GUARD_LINES")); err == nil {
		limit = v
	}
	file := in.ToolInput.FilePath
	if limit <= 0 || file == "" || in.ToolInput.Offset > 0 || in.ToolInput.Limit > 0 {
		return nil
	}
	lines := countLines(file, limit)
	if lines < limit {
		return nil
	}
	state := stateFile(in.SessionID)
	if state == "" {
		return nil
	}
	s := loadState(state)
	if slices.Contains(s.Read, file) {
		return nil // asked to read it again: let it
	}
	s.Read = append(s.Read, file)
	s.Turned++
	saveState(state, s)
	var b strings.Builder
	fmt.Fprintf(&b, "%s has %d+ lines. Read only the ranges you need (Read with offset and limit), or call file_outline for its symbols with signatures and line ranges.", filepath.Base(file), lines)
	if outline := goOutline(file); len(outline) > 0 {
		b.WriteString(" Outline:\n" + strings.Join(outline, "\n"))
	}
	b.WriteString("\nTo read the whole file anyway, read it again.")
	return map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": b.String(),
	}}
}

// countLines counts up to stop lines (it only needs to know "large").
func countLines(file string, stop int) int {
	f, err := os.Open(file) //nolint:gosec // the file the agent is about to read
	if err != nil {
		return 0
	}
	defer f.Close() //nolint:errcheck // read-only
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	n := 0
	for n < stop && sc.Scan() {
		n++
	}
	return n
}

// goOutline lists a Go file's top-level declarations with line ranges, from
// the syntax alone (fast; no type checking).
func goOutline(file string) []string {
	if filepath.Ext(file) != ".go" {
		return nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	var out []string
	add := func(name string, from, to token.Pos) {
		if len(out) < maxOutline {
			out = append(out, fmt.Sprintf("  %s: lines %d-%d", name, fset.Position(from).Line, fset.Position(to).Line))
		}
	}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				name = "(" + exprString(d.Recv.List[0].Type) + ")." + name
			}
			add("func "+name, d.Pos(), d.End())
		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				continue
			}
			for _, sp := range d.Specs {
				if ts, ok := sp.(*ast.TypeSpec); ok {
					add("type "+ts.Name.Name, d.Pos(), d.End())
				}
			}
		}
	}
	return out
}

func exprString(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.StarExpr:
		return "*" + exprString(e.X)
	case *ast.Ident:
		return e.Name
	case *ast.IndexExpr:
		return exprString(e.X)
	case *ast.IndexListExpr:
		return exprString(e.X)
	}
	return "?"
}

// stopGuard asks for a check once per state of the working tree.
func stopGuard(ctx context.Context, in toolHookInput, logger *slog.Logger) map[string]any {
	if in.StopHookActive || in.Cwd == "" {
		return nil // already continuing because of a stop hook
	}
	w := newWorktree(ctx, in.Cwd, "", logger)
	if w == nil {
		return nil
	}
	change := stopChanges(ctx, w.root)
	if change.Key == "" {
		return nil // nothing CodeSynapse can say anything about
	}
	state := stateFile(in.SessionID)
	if state == "" {
		return nil
	}
	s := loadState(state)
	if s.Stop == change.Key {
		return nil // this tree was checked already, or is how the session began
	}
	s.Stop = change.Key
	saveState(state, s)
	reason := fmt.Sprintf("Before you finish: %d file(s) in %s changed since CodeSynapse last checked. "+
		"Call verify_plan (no arguments: your current change) and run the commands it lists; "+
		"call get_pr_context (no arguments) and fix any finding its security_delta lists as introduced. "+
		"Then finish. If the CodeSynapse tools are not available, finish now.", change.Files, w.repo)
	if len(change.Problems) > 0 {
		// Found by parsing and formatting the changed Go files here.
		reason = "Fix these first:\n- " + strings.Join(change.Problems, "\n- ") + "\n\n" + reason
	}
	return map[string]any{"decision": "block", "reason": reason}
}

func hookCmd(ec *app.Env) *cobra.Command {
	parent := &cobra.Command{Use: "hook", Short: "Assistant lifecycle hooks for CodeSynapse", GroupID: app.GroupBase}
	for _, event := range []string{"edit", "edit-failed"} {
		parent.AddCommand(app.NewSubCommand(event, &hookOptions{EC: ec, event: event}, ec).
			WithShortDesc("Observe edit/write payload sizes without storing source or blocking edits").WithNoArgs().Build())
	}
	parent.AddCommand(app.NewSubCommand("lifecycle", &lifecycleOptions{EC: ec}, ec).
		WithShortDesc("Codex and Cursor lifecycle adapter: rules, guards and metadata-only activity").WithNoArgs().Build())
	parent.AddCommand(app.NewSubCommand("read", &hookOptions{EC: ec, event: "read"}, ec).
		WithShortDesc("PreToolUse hook for Read: turn back the first whole read of a large file with its outline").
		WithNoArgs().Build())
	parent.AddCommand(app.NewSubCommand("stop", &hookOptions{EC: ec, event: "stop"}, ec).
		WithShortDesc("Stop hook: report retrieval counts; once per change, ask for verify_plan and get_pr_context before finishing").
		WithNoArgs().Build())
	parent.AddCommand(app.NewSubCommand("session", &hookOptions{EC: ec, event: "session"}, ec).
		WithShortDesc("SessionStart hook: put the CodeSynapse ruleset (from the server, else built in) in the session's context").
		WithNoArgs().Build())
	parent.AddCommand(app.NewSubCommand("lookup", &hookOptions{EC: ec, event: "lookup"}, ec).
		WithShortDesc("PreToolUse hook for Read, Grep, Glob and the CodeSynapse tools: note how the session began").
		WithNoArgs().Build())
	parent.AddCommand(app.NewSubCommand("bash", &hookOptions{EC: ec, event: "bash"}, ec).
		WithShortDesc("PreToolUse hook for Bash: hint at the CodeSynapse tool that answers git, search, file reads and test runs with less output").
		WithNoArgs().Build())
	parent.AddCommand(app.NewSubCommand("answer", &hookOptions{EC: ec, event: "answer"}, ec).
		WithShortDesc("PostToolUse hook for the CodeSynapse tools: note the line ranges an answer gave").
		WithNoArgs().Build())
	parent.AddCommand(app.NewSubCommand("prompt", &hookOptions{EC: ec, event: "prompt"}, ec).
		WithShortDesc("UserPromptSubmit hook: with CODESYNAPSE_EVAL_CAPTURE=true, keep the session's first prompt for an eval case").
		WithNoArgs().Build())
	parent.AddCommand(app.NewSubCommand("read-done", &hookOptions{EC: ec, event: "read-done"}, ec).
		WithShortDesc("PostToolUse hook for Read: count the answered line ranges the agent read").
		WithNoArgs().Build())
	return parent
}
