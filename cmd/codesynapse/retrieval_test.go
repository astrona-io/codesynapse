package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/taskeval"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestRetrievalCounts: ranges an answer gave are counted per tool, a Read
// that covers one counts it read, and Stop reports the counts once, with no
// path in the report.
func TestRetrievalCounts(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	var reports []string
	var paths []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		reports, paths = append(reports, string(body)), append(paths, r.URL.EscapedPath())
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(api.Close)
	t.Setenv("CODESYNAPSE_API_URL", api.URL)
	t.Setenv(apiTokenEnv, "csk_test")
	dir := gitRepo(t)

	answer := func(tool, repo, text string) {
		t.Helper()
		in := toolHookInput{SessionID: "chat-1", Cwd: dir, ToolName: tool}
		in.ToolInput.Repo = repo
		resp, err := json.Marshal([]map[string]any{{"type": "text", "text": text}})
		require.NoError(t, err)
		in.ToolResponse = resp
		noteAnswer(in)
	}
	answer("mcp__plugin_codesynapse_codesynapse__get_symbol_context", "",
		`{"symbol":{"location":"internal/svc/svc.go:10-20"},"tests":[{"location":"internal/svc/svc_test.go:5-9"}],"see":"https://example.com:443/x"}`)
	answer("mcp__codesynapse__search", "", `{"results":[{"location":"internal/svc/svc.go:10-20"},{"location":"web/app.ts:3"}]}`)
	answer("mcp__codesynapse__search", "other/repo", `{"results":[{"location":"lib/x.go:1-2"}]}`)
	answer("mcp__github__search", "", `{"results":[{"location":"a/b.go:1"}]}`)

	read := func(file string, offset, limit int) {
		in := toolHookInput{SessionID: "chat-1", Cwd: dir}
		in.ToolInput.FilePath, in.ToolInput.Offset, in.ToolInput.Limit = filepath.Join(dir, file), offset, limit
		noteRead(in)
	}
	read("internal/svc/svc.go", 15, 40) // overlaps 10-20
	read("web/app.ts", 0, 0)            // the whole file
	read("internal/svc/svc_test.go", 50, 10)

	stop := toolHookInput{SessionID: "chat-1", Cwd: dir}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reportRetrieval(context.Background(), stop, logger)
	require.Len(t, reports, 1)
	assert.Equal(t, paths[0], "/api/v1/repos/acme%2Fdemo/usage/retrieval")
	var got struct {
		Client string                 `json:"client"`
		Tools  map[string]rangeCounts `json:"tools"`
	}
	require.NoError(t, json.Unmarshal([]byte(reports[0]), &got))
	assert.Equal(t, got.Client, "claude-code")
	assert.Equal(t, got.Tools, map[string]rangeCounts{
		"get_symbol_context": {Returned: 2, Read: 1},
		"search":             {Returned: 1, Read: 1},
	}, "a range given twice counts once; another repository's and another server's are left out")
	assert.NotContains(t, reports[0], "svc.go", "no paths leave the machine")

	reportRetrieval(context.Background(), stop, logger)
	assert.Len(t, reports, 1, "nothing new, nothing sent")
	read("internal/svc/svc_test.go", 1, 0)
	reportRetrieval(context.Background(), stop, logger)
	require.Len(t, reports, 2)
	assert.Equal(t, reports[1], `{"client":"claude-code","tools":{"get_symbol_context":{"returned":0,"read":1}}}`)
}

// TestEditRecall: of the files the session changed (not those already
// changed when it began, nor files CodeSynapse does not index), Stop counts
// how many an answer had pointed to, and reports only the counts.
func TestEditRecall(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	var reports []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		reports = append(reports, string(body))
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(api.Close)
	t.Setenv("CODESYNAPSE_API_URL", api.URL)
	t.Setenv(apiTokenEnv, "csk_test")
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := gitRepo(t)
	write := func(name, body string) {
		t.Helper()
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
	write("scratch.go", "package demo\n") // changed before the session: not its edit
	in := toolHookInput{SessionID: "chat-e", Cwd: dir}
	noteBaseline(ctx, in, logger)

	answer := toolHookInput{SessionID: "chat-e", Cwd: dir, ToolName: "mcp__codesynapse__minimal_context"}
	resp, err := json.Marshal([]map[string]any{{"type": "text", "text": `{"items":[{"read":"demo.go:3-4"}]}`}})
	require.NoError(t, err)
	answer.ToolResponse = resp
	noteAnswer(answer)

	write("demo.go", "package demo\n\nfunc Synced() int { return 2 }\n") // pointed to
	write("other.go", "package demo\n\nfunc Other() {}\n")               // not pointed to
	write("notes.txt", "not indexed\n")                                  // not counted
	reportRetrieval(ctx, in, logger)
	require.Len(t, reports, 1)
	assert.Contains(t, reports[0], `"files_edited":2`)
	assert.Contains(t, reports[0], `"files_edited_found":1`)
	assert.NotContains(t, reports[0], "demo.go", "no paths leave the machine")

	reportRetrieval(ctx, in, logger)
	assert.Len(t, reports, 1, "nothing new, nothing sent")

	// A commit of the session still counts its files; the scratch file,
	// now edited by the session, joins them.
	_, err = gitx.Run(ctx, dir, "add", "demo.go", "other.go")
	require.NoError(t, err)
	_, err = gitx.Run(ctx, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "edit")
	require.NoError(t, err)
	write("scratch.go", "package demo\n\nfunc Scratch() {}\n")
	reportRetrieval(ctx, in, logger)
	require.Len(t, reports, 2)
	assert.Contains(t, reports[1], `"files_edited":1`)
	assert.NotContains(t, reports[1], "files_edited_found")
}

// TestEvalCapture: with capture on, a session becomes a case in the
// checkout's git directory: its first prompt (redacted) and the files it
// changed that existed where it began. Without it, nothing is kept.
func TestEvalCapture(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := gitRepo(t)
	in := toolHookInput{SessionID: "chat-cap", Cwd: dir}
	cases := filepath.Join(dir, ".git", "codesynapse-eval-cases.jsonl")

	noteBaseline(ctx, in, logger)
	in.Prompt = "Make Synced return 2, mail ada@example.com when done"
	notePrompt(in)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "demo.go"), []byte("package demo\n\nfunc Synced() int { return 2 }\n"), 0o600))
	captureCase(ctx, in, logger)
	_, err := os.Stat(cases)
	assert.True(t, os.IsNotExist(err), "capture is opt-in")

	t.Setenv(evalCaptureEnv, "true")
	in2 := toolHookInput{SessionID: "chat-cap2", Cwd: dir}
	noteBaseline(ctx, in2, logger)
	in2.Prompt = "Make Synced return 3 (api_key=sk_live_9f8e7d6c5b4a), call +45 22 34 56 78 when done"
	notePrompt(in2)
	in2.Prompt = "a later prompt is not the task"
	notePrompt(in2)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "demo.go"), []byte("package demo\n\nfunc Synced() int { return 3 }\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "added.go"), []byte("package demo\n"), 0o600))
	captureCase(ctx, in2, logger)
	got, err := taskeval.ReadCases(cases)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, got[0].Truth, []string{"demo.go"})
	assert.Equal(t, got[0].Added, 1, "a file the session created cannot be in the graph at its start")
	assert.Contains(t, got[0].Task, "Make Synced return 3")
	assert.NotContains(t, got[0].Task, "sk_live_9f8e7d6c5b4a", "secrets are redacted")
	assert.NotContains(t, got[0].Task, "22 34 56 78", "and personal numbers")
	assert.NotContains(t, got[0].Session, "chat-cap2", "the session ID is hashed")
}

// TestEvalSessions scores captured cases against the graph at the commit
// each began at.
func TestEvalSessions(t *testing.T) {
	ctx := context.Background()
	dir := gitRepo(t)
	head := gitx.HeadCommit(ctx, dir)
	cases := filepath.Join(t.TempDir(), "cases.jsonl")
	require.NoError(t, taskeval.SaveCase(cases, taskeval.Case{Session: "s1", Task: "change what Synced returns", Base: head, Truth: []string{"demo.go"}, At: time.Now()}))
	require.NoError(t, taskeval.SaveCase(cases, taskeval.Case{Session: "s2", Task: "old work", Base: strings.Repeat("ab", 20), Truth: []string{"demo.go"}, At: time.Now()}))

	var out bytes.Buffer
	o := &evalOptions{EC: &app.Env{Stdout: &out, Output: app.OutputJSON, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, root: dir, sessions: true, cases: cases}
	require.NoError(t, o.Validate(ctx, nil))
	require.NoError(t, o.Run(ctx, nil))
	var rep taskeval.Report
	require.NoError(t, json.Unmarshal(out.Bytes(), &rep))
	assert.Equal(t, rep.Tasks, 1, "a case of a commit this checkout lacks is skipped")
	assert.Equal(t, rep.Summary[taskeval.ToolMinimalContext].Recall10, 1.0)

	bad := &evalOptions{EC: &app.Env{}, sessions: true, base: "HEAD~1"}
	assert.True(t, bad.Validate(ctx, nil) != nil, "--sessions does not mix with --base")
	empty := &evalOptions{EC: &app.Env{Stdout: &out, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, root: dir, sessions: true, cases: filepath.Join(t.TempDir(), "none.jsonl")}
	err := empty.Run(ctx, nil)
	require.True(t, err != nil)
	assert.Contains(t, err.Error(), evalCaptureEnv)
}

// TestRetrievalRepoRenamed: a remote that names the old owner of a moved
// repository must not stop the report; the server's own name for the
// checkout is used instead.
func TestRetrievalRepoRenamed(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	var reported []string
	dir := gitRepo(t) // origin acme/demo; the server knows the directory name under the new owner
	name := "newowner/" + filepath.Base(dir)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos":
			_, _ = io.WriteString(w, `{"repos":[{"name":"`+name+`","remote":"https://github.com/`+name+`.git"}]}`) //nolint:errcheck // test server
		case r.URL.EscapedPath() == "/api/v1/repos/"+url.PathEscape(name)+"/usage/retrieval":
			reported = append(reported, r.URL.EscapedPath())
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"detail":"no such repository"}`) //nolint:errcheck // test server
		}
	}))
	t.Cleanup(api.Close)
	t.Setenv("CODESYNAPSE_API_URL", api.URL)
	t.Setenv(apiTokenEnv, "csk_test")

	in := toolHookInput{SessionID: "chat-2", Cwd: dir, ToolName: "mcp__codesynapse__search"}
	noteLookup(in)
	reportRetrieval(context.Background(), toolHookInput{SessionID: "chat-2", Cwd: dir}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	assert.Equal(t, len(reported), 1, "the report reached the server under its own name")
}
