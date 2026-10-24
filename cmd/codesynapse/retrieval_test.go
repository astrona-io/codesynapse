package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/astrona-io/codesynapse/internal/gitx"
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
