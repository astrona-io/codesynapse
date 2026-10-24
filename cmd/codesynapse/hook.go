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

	"github.com/spf13/cobra"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/execution"
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

// readGuardLines is how many lines make a file large enough to guard;
// CODESYNAPSE_READ_GUARD_LINES overrides it and 0 turns the guard off.
const readGuardLines = 400

// maxOutline bounds the outline a turned-back read gets.
const maxOutline = 60

type toolHookInput struct {
	SessionID      string `json:"session_id"`
	Cwd            string `json:"cwd"`
	StopHookActive bool   `json:"stop_hook_active"`
	ToolInput      struct {
		FilePath string `json:"file_path"`
		Offset   int    `json:"offset"`
		Limit    int    `json:"limit"`
	} `json:"tool_input"`
}

// hookState is what the hooks remember per chat session.
type hookState struct {
	Read []string `json:"read,omitempty"` // files already turned back once
	Stop string   `json:"stop,omitempty"` // the tree the last stop check saw
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
	_ = json.NewDecoder(io.LimitReader(o.EC.Stdin, 1<<20)).Decode(&in) //nolint:errcheck // no input, no opinion
	var out map[string]any
	switch o.event {
	case "read":
		out = readGuard(in)
	case "stop":
		out = stopGuard(ctx, in, o.EC.Logger)
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
	key, files, err := w.changes(ctx)
	if err != nil || key == "" {
		return nil
	}
	state := stateFile(in.SessionID)
	if state == "" {
		return nil
	}
	s := loadState(state)
	if s.Stop == key {
		return nil // this tree was checked already
	}
	s.Stop = key
	saveState(state, s)
	return map[string]any{
		"decision": "block",
		"reason": fmt.Sprintf("Before you finish: %d file(s) in %s changed since CodeSynapse last checked. "+
			"Call verify_plan (no arguments: your current change) and run the commands it lists; "+
			"call get_pr_context (no arguments) and fix any finding its security_delta lists as introduced. "+
			"Then finish. If the CodeSynapse tools are not available, finish now.", files, w.repo),
	}
}

func hookCmd(ec *app.Env) *cobra.Command {
	parent := &cobra.Command{Use: "hook", Short: "Claude Code hooks of the CodeSynapse plugin", GroupID: app.GroupBase}
	parent.AddCommand(app.NewSubCommand("read", &hookOptions{EC: ec, event: "read"}, ec).
		WithShortDesc("PreToolUse hook for Read: turn back the first whole read of a large file with its outline").
		WithNoArgs().Build())
	parent.AddCommand(app.NewSubCommand("stop", &hookOptions{EC: ec, event: "stop"}, ec).
		WithShortDesc("Stop hook: once per change, ask for verify_plan and get_pr_context before finishing").
		WithNoArgs().Build())
	return parent
}
