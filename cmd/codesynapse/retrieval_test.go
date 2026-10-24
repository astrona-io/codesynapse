package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

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
