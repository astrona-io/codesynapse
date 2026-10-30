package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// The gateway benchmark (bench_test.go) sends these requests. They are shaped
// like Claude Code's: a long system prompt in cache-controlled blocks, its
// usual tools with JSON schemas, system reminders in the first user turn,
// tool_use/tool_result rounds and signed thinking in earlier assistant turns.
// Every word and every line of "code" in them is made up here, about a
// fictional project; nothing comes from a real conversation or repository.
//
// The files in testdata/gatewaybench are generated from this code so that
// they stay reviewable and reproducible:
//
//	CODESYNAPSE_GATEWAY_BENCH_WRITE=1 go test -run TestBenchFixtures ./services/api/internal/gateway/
const benchDir = "testdata/gatewaybench"

const (
	benchMain  = "claude-opus-5-5"
	benchSmall = "claude-haiku-4-5"
)

// benchFixture is one request with a stable name; the name is the only thing
// about it the benchmark reports.
type benchFixture struct {
	Name string
	Body map[string]any
}

func benchFixtures() []benchFixture {
	return []benchFixture{
		{"small-title", smallTitle()},
		{"small-quota", smallQuota()},
		{"main-first-turn", mainRequest(firstTurn())},
		{"main-after-grep", mainRequest(afterGrep())},
		{"main-after-read", mainRequest(afterRead())},
		{"main-tool-error", mainRequest(afterToolError())},
		{"main-final-answer", mainRequest(afterTests())},
		{"main-question", mainRequest(benchQuestion())},
		{"main-large-context", mainRequest(largeContext())},
	}
}

func smallTitle() map[string]any {
	return map[string]any{
		"model":      benchSmall,
		"max_tokens": 512,
		"stream":     true,
		"system": []any{benchText("Decide whether the user's latest message starts a new topic in this coding session. " +
			"Answer with a JSON object with two fields: isNewTopic (boolean) and title (a title of two to four words, or null when the topic is unchanged). " +
			"Answer with the JSON object only.")},
		"messages": []any{benchUserText("The stock reservation endpoint returns 500 when two orders reserve the last item at the same time. Can you find out why and fix it?")},
		"metadata": map[string]any{"user_id": "bench-user"},
	}
}

func smallQuota() map[string]any {
	return map[string]any{
		"model":      benchSmall,
		"max_tokens": 1,
		"messages":   []any{benchUserText("quota")},
		"metadata":   map[string]any{"user_id": "bench-user"},
	}
}

func mainRequest(messages []any) map[string]any {
	return map[string]any{
		"model":      benchMain,
		"max_tokens": 32000,
		"stream":     true,
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": 31999},
		"system":     benchSystem(),
		"tools":      benchTools(),
		"messages":   messages,
		"metadata":   map[string]any{"user_id": "bench-user"},
	}
}

func benchText(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

func benchCached(s string) map[string]any {
	return map[string]any{"type": "text", "text": s, "cache_control": map[string]any{"type": "ephemeral"}}
}

func benchUserText(s string) map[string]any {
	return map[string]any{"role": "user", "content": s}
}

func benchUser(blocks ...any) map[string]any {
	return map[string]any{"role": "user", "content": blocks}
}

func benchAssistant(blocks ...any) map[string]any {
	return map[string]any{"role": "assistant", "content": blocks}
}

func benchThinking(s string) map[string]any {
	// The signature is a made-up opaque string of the length Claude sends.
	return map[string]any{"type": "thinking", "thinking": s, "signature": strings.Repeat("U2lnbmF0dXJlQmVuY2g", 24)}
}

func benchToolUse(id, name string, input map[string]any) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}
}

func benchToolResult(id, content string, isError bool) map[string]any {
	r := map[string]any{"type": "tool_result", "tool_use_id": id, "content": content}
	if isError {
		r["is_error"] = true
	}
	return r
}

const benchTask = "The stock reservation endpoint returns 500 when two orders reserve the last item at the same time. " +
	"Find out why and fix it so the second order gets a 409 with a clear message. Keep the change small and add a test."

func benchReminder() map[string]any {
	return benchText("<system-reminder>\nAs you answer the user's questions, you can use the following context:\n" +
		"# projectInstructions\nContents of /work/lumen/NOTES.md (project instructions, checked into the codebase):\n\n" +
		"Lumen is a small inventory service. Handlers live in api/, storage in store/, domain rules in stock/. " +
		"Run `make test` before you finish. Errors are wrapped with context; never log customer addresses. " +
		"Tests are table-driven and use the helpers in internal/check.\n\n" +
		"IMPORTANT: this context may or may not be relevant to your tasks. You should not respond to this context unless it is highly relevant to your task.\n</system-reminder>")
}

func firstTurn() []any {
	return []any{benchUser(benchReminder(), benchText(benchTask))}
}

func afterGrep() []any {
	return append(firstTurn(),
		benchAssistant(
			benchThinking("The 500 suggests an unhandled error from the store when the quantity would go negative. I should find where reservations are written."),
			benchText("I'll look for where reservations are stored."),
			benchToolUse("toolu_bench_01", "Grep", map[string]any{"pattern": "func .*Reserve", "path": "/work/lumen", "output_mode": "content", "-n": true}),
		),
		benchUser(benchToolResult("toolu_bench_01", "/work/lumen/stock/reserve.go:41:func (s *Service) Reserve(ctx context.Context, sku string, qty int) (Reservation, error) {\n"+
			"/work/lumen/store/pg.go:118:func (db *DB) ReserveStock(ctx context.Context, sku string, qty int) (int, error) {\n"+
			"/work/lumen/api/reserve.go:22:func (h *Handler) Reserve(w http.ResponseWriter, r *http.Request) {", false)),
	)
}

func afterRead() []any {
	return append(afterGrep(),
		benchAssistant(
			benchText("Let me read the service and the store method."),
			benchToolUse("toolu_bench_02", "Read", map[string]any{"file_path": "/work/lumen/stock/reserve.go"}),
			benchToolUse("toolu_bench_03", "Read", map[string]any{"file_path": "/work/lumen/store/pg.go", "offset": 100, "limit": 60}),
		),
		benchUser(
			benchToolResult("toolu_bench_02", numbered(reserveFile(), 1), false),
			benchToolResult("toolu_bench_03", numbered(storeExcerpt(), 100), false),
		),
	)
}

func afterToolError() []any {
	return append(afterRead(),
		benchAssistant(
			benchThinking("ReserveStock returns pgErrCheck when the CHECK constraint fails, and Reserve wraps it without mapping it to ErrOutOfStock. I'll map it in the service."),
			benchText("The store reports the failed CHECK constraint as a raw error. I'll map it to ErrOutOfStock in the service."),
			benchToolUse("toolu_bench_04", "Edit", map[string]any{
				"file_path":  "/work/lumen/stock/reserve.go",
				"old_string": "\tleft, err := s.db.ReserveStock(ctx, sku, qty)\n\tif err != nil {\n\t\treturn Reservation{}, fmt.Errorf(\"reserve %s: %w\", sku, err)\n\t}",
				"new_string": "\tleft, err := s.db.ReserveStock(ctx, sku, qty)\n\tif errors.Is(err, store.ErrCheck) {\n\t\treturn Reservation{}, ErrOutOfStock\n\t}\n\tif err != nil {\n\t\treturn Reservation{}, fmt.Errorf(\"reserve %s: %w\", sku, err)\n\t}",
			}),
		),
		benchUser(benchToolResult("toolu_bench_04", "The file /work/lumen/stock/reserve.go has been updated.", false)),
		benchAssistant(
			benchToolUse("toolu_bench_05", "Bash", map[string]any{"command": "cd /work/lumen && go build ./...", "description": "Build all packages"}),
		),
		benchUser(benchToolResult("toolu_bench_05", "# lumen/stock\nstock/reserve.go:47:12: undefined: errors\nstock/reserve.go:47:26: undefined: store.ErrCheck", true)),
	)
}

func afterTests() []any {
	return append(afterRead(),
		benchAssistant(
			benchThinking("I'll add ErrCheck to the store, map it in the service, map ErrOutOfStock to 409 in the handler and add a table case."),
			benchToolUse("toolu_bench_06", "Edit", map[string]any{
				"file_path":  "/work/lumen/stock/reserve.go",
				"old_string": "\tif err != nil {\n\t\treturn Reservation{}, fmt.Errorf(\"reserve %s: %w\", sku, err)\n\t}",
				"new_string": "\tif errors.Is(err, store.ErrCheck) {\n\t\treturn Reservation{}, ErrOutOfStock\n\t}\n\tif err != nil {\n\t\treturn Reservation{}, fmt.Errorf(\"reserve %s: %w\", sku, err)\n\t}",
			}),
		),
		benchUser(benchToolResult("toolu_bench_06", "The file /work/lumen/stock/reserve.go has been updated.", false)),
		benchAssistant(
			benchToolUse("toolu_bench_07", "Bash", map[string]any{"command": "cd /work/lumen && make test", "description": "Run the test suite"}),
		),
		benchUser(benchToolResult("toolu_bench_07", testOutput(), false)),
	)
}

func benchQuestion() []any {
	return []any{benchUser(benchReminder(), benchText("Before we change anything: in two or three sentences, why would a CHECK constraint on stock.quantity be a better guard against overselling than reading the quantity first and then updating it?"))}
}

func largeContext() []any {
	msgs := afterGrep()
	var uses, results []any
	for i, name := range []string{"orders", "pricing", "shipping", "audit"} {
		id := fmt.Sprintf("toolu_bench_l%d", i)
		uses = append(uses, benchToolUse(id, "Read", map[string]any{"file_path": "/work/lumen/" + name + "/" + name + ".go"}))
		results = append(results, benchToolResult(id, numbered(syntheticFile(name, 90), 1), false))
	}
	msgs = append(msgs,
		benchAssistant(append([]any{benchText("Let me read the packages that call Reserve.")}, uses...)...),
		benchUser(append(results, benchText("Which of these packages call Reserve without handling ErrOutOfStock? List them with the line numbers."))...),
	)
	return msgs
}

// numbered formats lines like Claude Code's Read tool.
func numbered(src string, from int) string {
	var b strings.Builder
	for i, line := range strings.Split(src, "\n") {
		fmt.Fprintf(&b, "%6d\t%s\n", from+i, line)
	}
	return b.String()
}

func reserveFile() string {
	return `package stock

import (
	"context"
	"fmt"
	"time"

	"lumen/store"
)

// ErrOutOfStock means the item cannot cover the requested quantity.
var ErrOutOfStock = fmt.Errorf("not enough stock")

// Reservation holds items for an order until it is paid or expires.
type Reservation struct {
	SKU     string
	Qty     int
	Left    int
	Expires time.Time
}

// Service applies the stock rules.
type Service struct {
	db  *store.DB
	ttl time.Duration
}

// New returns a Service whose reservations expire after ttl.
func New(db *store.DB, ttl time.Duration) *Service {
	return &Service{db: db, ttl: ttl}
}

// Reserve takes qty items of sku out of the free stock.
func (s *Service) Reserve(ctx context.Context, sku string, qty int) (Reservation, error) {
	if qty <= 0 {
		return Reservation{}, fmt.Errorf("reserve %s: quantity must be positive", sku)
	}
	left, err := s.db.ReserveStock(ctx, sku, qty)
	if err != nil {
		return Reservation{}, fmt.Errorf("reserve %s: %w", sku, err)
	}
	return Reservation{SKU: sku, Qty: qty, Left: left, Expires: time.Now().Add(s.ttl)}, nil
}

// Release returns a reservation's items to the free stock.
func (s *Service) Release(ctx context.Context, r Reservation) error {
	if err := s.db.ReleaseStock(ctx, r.SKU, r.Qty); err != nil {
		return fmt.Errorf("release %s: %w", r.SKU, err)
	}
	return nil
}`
}

func storeExcerpt() string {
	return `// ReserveStock subtracts qty from the free quantity and returns what is left.
// The quantity column has CHECK (quantity >= 0), so a reservation that would
// oversell fails in the database rather than here.
func (db *DB) ReserveStock(ctx context.Context, sku string, qty int) (int, error) {
	var left int
	err := db.pool.QueryRow(ctx,
		"UPDATE stock SET quantity = quantity - $2 WHERE sku = $1 RETURNING quantity",
		sku, qty).Scan(&left)
	if err != nil {
		return 0, fmt.Errorf("update stock: %w", err)
	}
	return left, nil
}

// ReleaseStock adds qty back to the free quantity.
func (db *DB) ReleaseStock(ctx context.Context, sku string, qty int) error {
	_, err := db.pool.Exec(ctx, "UPDATE stock SET quantity = quantity + $2 WHERE sku = $1", sku, qty)
	if err != nil {
		return fmt.Errorf("release stock: %w", err)
	}
	return nil
}`
}

func testOutput() string {
	var b strings.Builder
	for _, pkg := range []string{"api", "orders", "pricing", "shipping", "stock", "store", "audit"} {
		fmt.Fprintf(&b, "ok  \tlumen/%s\t0.%03ds\n", pkg, 100+len(pkg)*37)
	}
	b.WriteString("PASS")
	return b.String()
}

// syntheticFile writes n made-up functions in package name; each reads like
// ordinary service code so the model sees code-shaped text of a known size.
func syntheticFile(name string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n\nimport (\n\t\"context\"\n\t\"errors\"\n\t\"fmt\"\n\n\t\"lumen/stock\"\n)\n", name)
	for i := range n {
		fmt.Fprintf(&b, `
// Step%[2]d handles one %[1]s rule for an order line.
func (s *Service) Step%[2]d(ctx context.Context, line Line) (Result, error) {
	if line.Qty <= 0 {
		return Result{}, fmt.Errorf("%[1]s step %[2]d: quantity %%d", line.Qty)
	}
`, name, i)
		if i%7 == 3 {
			fmt.Fprintf(&b, `	r, err := s.stock.Reserve(ctx, line.SKU, line.Qty)
	if err != nil {
		return Result{}, fmt.Errorf("%[1]s step %[2]d: %%w", err)
	}
	line.Reserved = r.Qty
`, name, i)
		} else if i%11 == 5 {
			fmt.Fprintf(&b, `	r, err := s.stock.Reserve(ctx, line.SKU, line.Qty)
	if errors.Is(err, stock.ErrOutOfStock) {
		return Result{Status: StatusBackorder}, nil
	}
	if err != nil {
		return Result{}, err
	}
	line.Reserved = r.Qty
`)
		}
		fmt.Fprintf(&b, `	total := line.Price * int64(line.Qty)
	if total > s.limits.Max%[1]d {
		return Result{}, errors.New("%[2]s: over the limit")
	}
	return Result{Total: total, Status: StatusOK}, nil
}
`, i%4, name)
	}
	return strings.TrimRight(b.String(), "\n")
}

// benchSystem is a made-up system prompt of Claude Code's size and shape:
// identity, tone, task rules, tool rules, git rules and an environment block.
func benchSystem() []any {
	sections := []string{
		"You are a coding assistant that works in the user's terminal. You help with software engineering tasks: fixing bugs, adding features, explaining code, refactoring and reviewing changes. Use the instructions below and the tools available to you.",
		"# Tone and style\nBe concise and direct. Answer in fewer than four lines unless the user asks for detail or the task needs it. Do not add a preamble or a summary of what you just did unless asked. When you run a command that changes the system, say briefly what it does and why. Use GitHub-flavoured Markdown; it is rendered in a monospace font. Never use emojis unless the user asks for them. Output text to talk to the user; never use tool calls or code comments as a way to talk to the user.",
		"# Being proactive\nDo what the user asked, including reasonable follow-up actions, but do not surprise them with changes they did not ask for. When the user asks how to approach something, answer the question first rather than starting to change files. Do not add explanations of your code changes unless the user asks.",
		"# Following conventions\nBefore you change a file, understand its conventions: mimic its style, use the libraries and helpers it already uses and follow its patterns. Never assume a library is available, even a well-known one; check the dependency manifest or neighbouring files first. When you create a component, look at existing ones first. When you edit code, look at its imports and surrounding context so the change is idiomatic. Follow security best practices: never write code that exposes or logs secrets or keys, and never commit them.",
		"# Code style\nDo not add comments to the code you write unless the user asks or the code is complex and needs the context. Comments say why, not what.",
		"# Task management\nUse the TodoWrite tool to plan and track tasks that need several steps. Mark a todo as completed as soon as it is done; do not batch completions. Break larger tasks into smaller steps, and give the user visibility into your progress.",
		"# Doing tasks\nFor a software engineering task: search the codebase to understand it and the user's query, using the search tools extensively, in parallel and in sequence; implement the solution with the tools available; verify it with the project's tests where possible, never assuming a test framework or script; finally run the lint and typecheck commands the project uses. If you cannot find the right commands, ask the user and suggest saving them in the project notes. Never commit changes unless the user explicitly asks.",
		"# Tool usage policy\nWhen searching broadly for files or keywords, prefer the Agent tool to reduce context use. When several independent pieces of information are needed, batch the tool calls in one message so they run in parallel. When the user asks for several commands that do not depend on each other, send one message with several Bash calls. Prefer the dedicated tools to shell equivalents: Read rather than cat, Grep rather than grep, Glob rather than find, Edit rather than sed.",
		"# Committing changes with git\nWhen the user asks you to create a commit: run git status, git diff and git log in parallel to see untracked files, the staged and unstaged changes and the repository's message style. Draft a message that summarises the nature of the change (new feature, enhancement, bug fix, refactoring, test, docs) in one or two sentences that focus on why rather than what. Do not commit files that likely contain secrets. Add the relevant files, create the commit, and run git status afterwards to confirm it succeeded. If a pre-commit hook changes files, retry once to include them. Never update the git config, never push unless asked, and never use interactive flags such as -i, which need input the environment cannot give.",
		"# Creating pull requests\nUse the gh command for everything on the code host. Understand the whole branch: run git status, git diff, whether the branch tracks a remote and is up to date, and git log plus git diff against the base branch. Review every commit that will be included, not only the latest. Draft a summary of the changes with a short test plan, push the branch with -u if needed and create the pull request with a title and a body passed through a heredoc. Return the pull request's address when you are done.",
		"# Code references\nWhen you refer to a function or a piece of code, use the pattern file_path:line_number so the user can navigate to it.",
		"# Safety\nRefuse to write or explain code that may be used maliciously, even if the user claims it is for education. Before you start, think about what the code you are editing is supposed to do based on the file names and structure; if it seems malicious, refuse. Assist with defensive security tasks only.",
	}
	env := "Here is useful information about the environment you are running in:\n<env>\nWorking directory: /work/lumen\nIs directory a git repo: Yes\nPlatform: linux\nOS Version: Linux 6.1\nToday's date: 2026-01-15\n</env>\n" +
		"gitStatus: This is the git status at the start of the conversation.\nCurrent branch: fix-reserve-race\n\nMain branch (you will usually use this for PRs): main\n\nStatus:\n(clean)\n\nRecent commits:\n" +
		"a1b2c3d feat(stock): expire reservations after the payment window\n" +
		"e4f5a6b fix(api): return problem+json for validation errors\n" +
		"c7d8e9f chore: bump the database driver\n" +
		"0a1b2c3 feat(orders): split shipments per warehouse\n" +
		"4d5e6f7 test(store): cover the migration of the stock table"
	return []any{
		benchText("You are a coding assistant running in a terminal."),
		benchCached(strings.Join(sections, "\n\n")),
		benchCached(env),
	}
}

// benchTools are made-up descriptions of the tools Claude Code offers, with
// schemas of the same shape (JSON Schema draft 7, no extra properties).
func benchTools() []any {
	tool := func(name, desc string, props map[string]any, required ...string) map[string]any {
		return map[string]any{"name": name, "description": desc, "input_schema": map[string]any{
			"$schema": "http://json-schema.org/draft-07/schema#", "type": "object",
			"additionalProperties": false, "properties": props, "required": required,
		}}
	}
	str := func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	num := func(d string) map[string]any { return map[string]any{"type": "number", "description": d} }
	boolean := func(d string) map[string]any { return map[string]any{"type": "boolean", "description": d} }
	tools := []any{
		tool("Agent", "Start a sub-agent that works on its own on a complex, multi-step task and reports back once. Use it for broad searches across many files, or for research whose intermediate results you do not need. The agent cannot see this conversation: describe the task fully, say what you already ruled out and what form the answer should take. Launch several agents in one message when their work is independent. The agent's result is not shown to the user; summarise what matters.",
			map[string]any{"description": str("A short description of the task in three to five words"), "prompt": str("The task for the agent to perform"), "subagent_type": str("The kind of agent to use")},
			"description", "prompt"),
		tool("Bash", "Run a bash command in a persistent shell session, with an optional timeout.\n\nBefore running a command that creates files or directories, check with ls that the parent directory exists. Quote paths that contain spaces with double quotes. The command may set a timeout in milliseconds of up to 600000; the default is 120000. Output longer than 30000 characters is truncated.\n\nAvoid find, grep, cat, head, tail, sed, awk and echo for reading and searching: use Glob, Grep and Read instead. When you run several commands, separate them with ; or &&, never with newlines. Keep the working directory stable by using absolute paths rather than cd.\n\nWrite a clear, concise description of what the command does in five to ten words.",
			map[string]any{"command": str("The command to execute"), "timeout": num("Optional timeout in milliseconds (max 600000)"), "description": str("Clear, concise description of what this command does in five to ten words"), "run_in_background": boolean("Run the command in the background and read its output later")},
			"command"),
		tool("Glob", "Fast file pattern matching that works with any codebase size. Supports glob patterns such as \"**/*.go\" or \"api/**/*.ts\". Returns matching file paths sorted by modification time. Use it when you need to find files by name.",
			map[string]any{"pattern": str("The glob pattern to match files against"), "path": str("The directory to search in; defaults to the working directory")},
			"pattern"),
		tool("Grep", "A search tool built on ripgrep. Always use Grep for search tasks; never run grep or rg through Bash. Supports full regular expression syntax, filtering by glob or file type, and three output modes: content shows matching lines, files_with_matches shows only paths (the default), count shows match counts. Literal braces need escaping. Patterns match within single lines unless multiline is set.",
			map[string]any{
				"pattern": str("The regular expression to search for in file contents"), "path": str("File or directory to search in; defaults to the working directory"),
				"glob": str("Glob pattern to filter files, such as \"*.go\""), "type": str("File type to search, such as go or py"),
				"output_mode": map[string]any{"type": "string", "enum": []string{"content", "files_with_matches", "count"}, "description": "Output mode; defaults to files_with_matches"},
				"-i":          boolean("Case insensitive search"), "-n": boolean("Show line numbers in the output; content mode only"),
				"-A": num("Lines to show after each match; content mode only"), "-B": num("Lines to show before each match; content mode only"),
				"head_limit": num("Limit the output to the first N lines or entries"), "multiline": boolean("Let patterns span lines"),
			},
			"pattern"),
		tool("Read", "Read a file from the local filesystem. The file_path must be absolute. By default it reads up to 2000 lines from the start of the file; for long files give an offset and a limit. Lines longer than 2000 characters are truncated. Results are returned with line numbers starting at 1. It can read images, PDFs and notebooks too. Read several files in one message when you need them all.",
			map[string]any{"file_path": str("The absolute path to the file to read"), "offset": num("The line number to start reading from"), "limit": num("The number of lines to read")},
			"file_path"),
		tool("Edit", "Perform an exact string replacement in a file. You must read the file in this conversation before editing it. Keep the indentation exactly as it appears after the line number prefix of the Read output, and never include the prefix itself in old_string or new_string. The edit fails when old_string is not unique in the file; give more surrounding context to make it unique, or set replace_all to change every instance.",
			map[string]any{"file_path": str("The absolute path to the file to modify"), "old_string": str("The text to replace"), "new_string": str("The text to replace it with; must differ from old_string"), "replace_all": boolean("Replace every occurrence of old_string")},
			"file_path", "old_string", "new_string"),
		tool("Write", "Write a file to the local filesystem, overwriting it if it exists. Read an existing file first. Prefer editing existing files; never create documentation files unless asked.",
			map[string]any{"file_path": str("The absolute path to the file to write"), "content": str("The content to write to the file")},
			"file_path", "content"),
		tool("TodoWrite", "Create and manage a structured task list for the current session. Use it for tasks with three or more distinct steps, when the user gives several tasks, or after new instructions arrive. Keep exactly one task in progress at a time and mark tasks completed as soon as they are done.",
			map[string]any{"todos": map[string]any{"type": "array", "description": "The updated todo list", "items": map[string]any{
				"type": "object", "additionalProperties": false, "required": []string{"content", "status", "id"},
				"properties": map[string]any{
					"content": map[string]any{"type": "string", "minLength": 1},
					"status":  map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "completed"}},
					"id":      map[string]any{"type": "string"},
				},
			}}},
			"todos"),
		tool("WebFetch", "Fetch a web page and answer a question about its content with a small model. The URL must be fully formed; http is upgraded to https. Results may be summarised when the page is large. Read-only.",
			map[string]any{"url": map[string]any{"type": "string", "format": "uri", "description": "The URL to fetch"}, "prompt": str("What to find out from the page")},
			"url", "prompt"),
	}
	// Sessions usually carry MCP tools too; these belong to two made-up servers.
	for _, m := range benchMCPTools {
		props := map[string]any{}
		for _, a := range m.args {
			props[a] = str("The " + strings.ReplaceAll(a, "_", " ") + " to use; see the tool description for the accepted values.")
		}
		tools = append(tools, tool(m.name, m.desc+" Answers are bounded; ask again with a narrower filter when the answer says it was cut.", props, m.args[0]))
	}
	return tools
}

var benchMCPTools = []struct {
	name, desc string
	args       []string
}{
	{"mcp__tracker__search_issues", "Search the issue tracker for issues matching a query, newest first, with their state, labels and assignee.", []string{"query", "state", "limit"}},
	{"mcp__tracker__get_issue", "Read one issue with its description, comments and linked changes.", []string{"issue_id"}},
	{"mcp__tracker__list_comments", "List the comments of an issue, oldest first, with author handles and timestamps.", []string{"issue_id", "limit"}},
	{"mcp__tracker__list_milestones", "List the project's milestones with their due dates and open issue counts.", []string{"project", "state"}},
	{"mcp__tracker__list_labels", "List the labels a project uses, with their descriptions.", []string{"project"}},
	{"mcp__tracker__get_pull_request", "Read one pull request: title, state, checks, reviewers and the files it changes.", []string{"number"}},
	{"mcp__tracker__list_pull_requests", "List pull requests by state, author or branch, newest first.", []string{"state", "author", "branch"}},
	{"mcp__tracker__get_check_run", "Read one CI check run with its failing steps and the last lines of each step's log.", []string{"run_id"}},
	{"mcp__tracker__list_releases", "List the project's releases with their tags and dates.", []string{"project", "limit"}},
	{"mcp__tracker__get_release_notes", "Read the notes of one release.", []string{"tag"}},
	{"mcp__tracker__search_code", "Search the default branch of every repository in the organisation for a literal string.", []string{"query", "language", "limit"}},
	{"mcp__tracker__get_file_history", "List the commits that changed a file, newest first, with author handles and subjects.", []string{"path", "limit"}},
	{"mcp__docs__search", "Search the team's internal documentation by meaning and return the best matching sections with their headings.", []string{"query", "space", "limit"}},
	{"mcp__docs__get_page", "Read one documentation page as Markdown.", []string{"page_id"}},
	{"mcp__docs__list_spaces", "List the documentation spaces and what each is for.", []string{"filter"}},
	{"mcp__docs__get_section", "Read one section of a documentation page by its heading.", []string{"page_id", "heading"}},
	{"mcp__docs__list_children", "List the pages below a page.", []string{"page_id"}},
	{"mcp__docs__recent_changes", "List documentation pages changed in the last days, newest first.", []string{"days", "space"}},
	{"mcp__docs__get_runbook", "Read the runbook for a service: alerts, dashboards, escalation and common fixes.", []string{"service"}},
	{"mcp__docs__get_glossary_term", "Explain a term from the team glossary.", []string{"term"}},
	{"mcp__docs__list_decisions", "List the architecture decision records with their status and date.", []string{"status", "limit"}},
	{"mcp__docs__get_decision", "Read one architecture decision record.", []string{"decision_id"}},
	{"mcp__docs__get_api_reference", "Read the reference of one internal HTTP endpoint: method, path, parameters, answers and errors.", []string{"service", "operation"}},
	{"mcp__docs__list_services", "List the services with their owners, languages and repositories.", []string{"team"}},
}

func encodeFixture(body map[string]any) ([]byte, error) {
	b, err := json.MarshalIndent(body, "", " ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// loadBenchFixtures reads the committed fixtures as the compact bodies Claude
// Code would send.
func loadBenchFixtures(t *testing.T) []benchFixture {
	t.Helper()
	var out []benchFixture
	for _, f := range benchFixtures() {
		raw, err := os.ReadFile(filepath.Join(benchDir, f.Name+".json"))
		require.NoError(t, err)
		var body map[string]any
		require.NoError(t, json.Unmarshal(raw, &body))
		out = append(out, benchFixture{Name: f.Name, Body: body})
	}
	return out
}

func compactBody(t *testing.T, body map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	return b
}

// TestBenchFixtures keeps the committed fixtures equal to their generator and
// checks the gateway would offer each one to a local model with a large
// context, so the benchmark measures answers rather than eligibility rules.
func TestBenchFixtures(t *testing.T) {
	write := os.Getenv("CODESYNAPSE_GATEWAY_BENCH_WRITE") == "1"
	if write {
		require.NoError(t, os.MkdirAll(benchDir, 0o755))
	}
	l := &Local{BaseURL: "http://127.0.0.1:1/v1", Model: "bench", ContextWindow: 262144}
	for _, f := range benchFixtures() {
		t.Run(f.Name, func(t *testing.T) {
			want, err := encodeFixture(f.Body)
			require.NoError(t, err)
			path := filepath.Join(benchDir, f.Name+".json")
			if write {
				require.NoError(t, os.WriteFile(path, want, 0o644))
			}
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.True(t, bytes.Equal(got, want), "%s is out of date: regenerate with CODESYNAPSE_GATEWAY_BENCH_WRITE=1", path)

			_, out, reason := l.translate(context.Background(), compactBody(t, f.Body))
			assert.Equal(t, "", reason)
			assert.True(t, out != nil, "the gateway does not offer %s to the local model", f.Name)
		})
	}
}
