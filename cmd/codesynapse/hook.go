package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/asklocal"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/usage"
)

// Claude Code hooks of the CodeSynapse plugin (docs/PLAN.md, agent workflow
// roadmap step 9; plugins/codesynapse). Like the memory hooks they never
// fail a session: any problem means "no opinion" and exit 0.
//
//   - hook read (PreToolUse on Read): the first time in a session the agent
//     reads all of a large file git tracks, it is turned back with the file's outline
//     and line ranges, and pointed at ask_local to have a local model read
//     it, so it reads only what it needs. Reading it again is
//     allowed: the guard nudges, it does not forbid.
//   - hook stop (Stop): when the session changed files since the last check,
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
	// AgentID is set when the hook fires inside a subagent (Claude Code).
	AgentID   string `json:"agent_id"`
	Source    string `json:"source"` // SessionStart: startup, resume, clear or compact
	ToolName  string `json:"tool_name"`
	ToolInput struct {
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
	// EditDirs are the directories the session's own agent edited files in
	// (stop.go), so Stop knows which checkouts it worked in.
	EditDirs []string `json:"edit_dirs,omitempty"`
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
		out = readGuard(ctx, in)
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
func readGuard(ctx context.Context, in toolHookInput) map[string]any {
	limit := readGuardLines
	if v, err := strconv.Atoi(os.Getenv("CODESYNAPSE_READ_GUARD_LINES")); err == nil {
		limit = v
	}
	file := in.ToolInput.FilePath
	if limit <= 0 || file == "" || in.ToolInput.Offset > 0 || in.ToolInput.Limit > 0 {
		return nil
	}
	lines := countLines(file, limit)
	if lines < limit || !guardable(ctx, file) {
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
	fmt.Fprintf(&b, "%s has %d+ lines. To learn what it says, call ask_local with your question and this path: a local model reads it and answers with file:line citations, so the file stays out of your context. To change it, read only the ranges you need (Read with offset and limit), or call file_outline for its symbols with signatures and line ranges.", filepath.Base(file), lines)
	if outline := goOutline(file); len(outline) > 0 {
		b.WriteString(" Outline:\n" + strings.Join(outline, "\n"))
	}
	b.WriteString("\nTo read the whole file anyway, read it again.")
	return map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": b.String(),
	}}
}

// guardable reports a file the turned-back read can point somewhere useful
// for: one git tracks (ask_local reads only those, and the graph indexes
// only those), and, unless CodeSynapse indexes its kind, small enough for
// ask_local. A saved tool output, a log or a scratch file outside a checkout
// is read as asked: nothing the guard suggests could answer for it.
func guardable(ctx context.Context, file string) bool {
	if !filepath.IsAbs(file) {
		return false
	}
	st, err := os.Stat(file)
	if err != nil || !st.Mode().IsRegular() || !indexable(file) && st.Size() > asklocal.MaxFileBytes {
		return false
	}
	_, err = gitx.Run(ctx, filepath.Dir(file), "ls-files", "--error-unmatch", "--", filepath.Base(file))
	return err == nil
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

// stopGuard asks for a check once per state of what the session changed. It
// asks only about the session's own work that the tools can answer for: in
// the checkout the session began in (the one the bridge answers for, so no
// arguments are needed) and in other worktrees of the same repository the
// session's agent edited (there the files are named, since no arguments
// would answer for the start checkout). Changes that were there when the
// session began, a start checkout someone else edits while the session works
// elsewhere, other repositories and subagent worktrees are left alone.
func stopGuard(ctx context.Context, in toolHookInput, logger *slog.Logger) map[string]any {
	if in.StopHookActive || in.Cwd == "" || in.AgentID != "" {
		return nil // already continuing because of a stop hook, or a subagent's
	}
	w := newWorktree(ctx, in.Cwd, "", logger)
	if w == nil {
		return nil
	}
	state := stateFile(in.SessionID)
	if state == "" {
		return nil
	}
	s := loadState(state)
	inStart, elsewhere, siblings := editedTrees(ctx, w.root, s.EditDirs)
	var change stopChange
	if inStart || !elsewhere {
		// A session that edited only other checkouts did not make the start
		// checkout's changes: the person works there meanwhile.
		change = stopChanges(ctx, w.root, s.Base)
	}
	type tree struct {
		root   string
		change stopChange
	}
	var others []tree
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00", change.Key) //nolint:errcheck // hashes never fail
	for _, root := range siblings {
		if c := stopChanges(ctx, root, nil); c.Key != "" {
			others = append(others, tree{root, c})
			_, _ = fmt.Fprintf(h, "%s\x00%s\x00", root, c.Key) //nolint:errcheck // hashes never fail
		}
	}
	if change.Key == "" && len(others) == 0 {
		return nil // nothing of this session CodeSynapse can say anything about
	}
	key := change.Key
	if len(others) > 0 {
		key = hex.EncodeToString(h.Sum(nil))
	}
	if s.Stop == key {
		return nil // this state was checked already, or is how the session began
	}
	s.Stop = key
	saveState(state, s)
	problems := change.Problems
	for _, t := range others {
		for _, p := range t.change.Problems {
			problems = append(problems, filepath.Base(t.root)+": "+p)
		}
	}
	repo := w.repo
	if name, known, indexing := serverRepo(ctx, in, w.root, w.repo); !known {
		// verify_plan and get_pr_context answer "not found" for a checkout the
		// server has not indexed, so asking for them only wastes a turn. Say so
		// in the chat (systemMessage) rather than finishing without a word.
		notice := fmt.Sprintf("CodeSynapse skipped its pre-finish check: %s is not in the server's index, so verify_plan and get_pr_context cannot answer for it. Add it to get the check.", w.repo)
		if indexing {
			notice = fmt.Sprintf("CodeSynapse skipped its pre-finish check: %s is queued or being indexed. verify_plan and get_pr_context answer for it once the ingest finishes.", w.repo)
		}
		if len(problems) == 0 {
			return map[string]any{"systemMessage": notice}
		}
		return map[string]any{
			"decision":      "block",
			"reason":        "Fix these first:\n- " + strings.Join(problems, "\n- "),
			"systemMessage": notice,
		}
	} else if name != "" {
		repo = name // the server may know the checkout by another name than its remote
	}
	var b strings.Builder
	if len(problems) > 0 {
		// Found by parsing and formatting the changed Go files here.
		b.WriteString("Fix these first:\n- " + strings.Join(problems, "\n- ") + "\n\n")
	}
	b.WriteString("Before you finish:")
	if change.Key != "" {
		fmt.Fprintf(&b, " %d file(s) in %s changed since CodeSynapse last checked. "+
			"Call verify_plan (no arguments: your current change) and run the commands it lists; "+
			"call get_pr_context (no arguments) and fix any finding its security_delta lists as introduced.", change.Files, repo)
	}
	for _, t := range others {
		// The bridge answers no-argument calls for the start checkout, so a
		// change in another worktree is named file by file.
		files, _ := json.Marshal(t.change.Paths) //nolint:errcheck // a list of strings
		fmt.Fprintf(&b, " %d file(s) of %s changed in the worktree %s, not in %s where this session started, "+
			"so calls with no arguments would check the wrong tree. Call verify_plan with files %s and run the commands it lists in %s; "+
			"call get_pr_context with the same files and fix any finding its security_delta lists as introduced.",
			t.change.Files, repo, t.root, w.root, files, t.root)
		if t.change.Files > len(t.change.Paths) {
			fmt.Fprintf(&b, " Only the first %d files are named.", len(t.change.Paths))
		}
	}
	b.WriteString(" Then finish. If the CodeSynapse tools are not available, finish now.")
	return map[string]any{"decision": "block", "reason": b.String()}
}

// serverRepo asks the server which indexed repository root is. known is false
// only when the server answered and does not list the checkout; with no token
// or no answer it stays true, since nothing says the tools are unavailable.
// checkout is the name the checkout's origin gives; a skipped check is reported
// under it (usage.Unindexed).
func serverRepo(ctx context.Context, in toolHookInput, root, checkout string) (name string, known, indexing bool) {
	c := newAPIClient(app.Getenv("CODESYNAPSE_API_URL", "http://127.0.0.1:8080"))
	if c.token == "" || in.SessionID == "" {
		return "", true, false
	}
	ctx, cancel := context.WithTimeout(ctx, activityBudget)
	defer cancel()
	ctx, err := c.executionContext(ctx, in.SessionID, in.SessionID)
	if err != nil {
		return "", true, false
	}
	name, err = c.resolveRepo(ctx, root)
	if errors.Is(err, errNoRepo) {
		if c.indexingNow(ctx, checkout) {
			// Already on its way in: nothing to recommend, so it is not counted.
			return "", false, true
		}
		// Counted so Statistics can recommend indexing it; best effort, the
		// person already gets the notice either way.
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), activityBudget)
		defer rcancel()
		_ = c.do(rctx, http.MethodPost, "/api/v1/usage/unindexed", map[string]string{"repo": checkout, "client": "claude-code"}, nil) //nolint:errcheck // counters are best effort
		return "", false, false
	}
	return name, true, false
}

// indexingNow reports whether a clone or ingest job for repo is queued or
// running. Any trouble asking says no: the person then gets the plain notice.
func (c apiClient) indexingNow(ctx context.Context, repo string) bool {
	var list struct {
		Jobs []struct {
			Repo  string `json:"repo"`
			State string `json:"state"`
		} `json:"jobs"`
	}
	if c.do(ctx, http.MethodGet, "/api/v1/jobs", nil, &list) != nil {
		return false
	}
	for _, j := range list.Jobs {
		if j.State == "running" && strings.EqualFold(j.Repo, repo) {
			return true
		}
	}
	return false
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
	parent.AddCommand(app.NewSubCommand("brief", &prebriefOptions{EC: ec}, ec).
		WithShortDesc("UserPromptSubmit hook: a local model briefs the agent on a code prompt before it starts (CODESYNAPSE_PREBRIEF=false turns it off)").
		WithNoArgs().Build())
	parent.AddCommand(app.NewSubCommand("read-done", &hookOptions{EC: ec, event: "read-done"}, ec).
		WithShortDesc("PostToolUse hook for Read: count the answered line ranges the agent read").
		WithNoArgs().Build())
	return parent
}
