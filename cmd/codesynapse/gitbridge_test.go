package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func gitCheckout(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.name", "Ada"}, {"config", "user.email", "ada@example.com"}, {"config", "commit.gpgsign", "false"}} {
		_, err := gitx.Run(context.Background(), dir, args...)
		require.NoError(t, err)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o600))
	_, err := gitx.Run(context.Background(), dir, "add", "-A")
	require.NoError(t, err)
	_, err = gitx.Run(context.Background(), dir, "commit", "-q", "-m", "first")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0o600))
	return dir
}

// The bridge answers the git tools from the checkout, never forwards them,
// and tells the server which tool ran and how large the answer was.
func TestBridgeAnswersGitToolsLocallyAndReportsThem(t *testing.T) {
	var mu sync.Mutex
	var reports []localUsage
	var auth string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, r.URL.Path, "/api/v1/usage/local")
		var u localUsage
		require.NoError(t, json.NewDecoder(r.Body).Decode(&u))
		mu.Lock()
		reports, auth = append(reports, u), r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(api.Close)
	var out bytes.Buffer
	b := &bridge{token: "csk_test", out: &out, gitDir: gitCheckout(t), logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		report: apiClient{base: api.URL, token: "csk_test", http: api.Client()}}

	call := func(id int, name, args string) map[string]any {
		out.Reset()
		msg := `{"jsonrpc":"2.0","id":` + string(rune('0'+id)) + `,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
		var m rpcMessage
		require.NoError(t, json.Unmarshal([]byte(msg), &m))
		require.True(t, b.answerGit(context.Background(), []byte(msg), m), name+" is answered here")
		var res struct {
			Result map[string]any `json:"result"`
		}
		require.NoError(t, json.Unmarshal(out.Bytes(), &res))
		return res.Result
	}

	st := call(1, "git_status", `{}`)
	assert.Equal(t, st["isError"], nil)
	sc := st["structuredContent"].(map[string]any)
	assert.Equal(t, sc["branch"], "main")
	assert.Equal(t, st["_meta"].(map[string]any)["codesynapse/source"], "local-git")
	files := sc["files"].([]any)
	require.Len(t, files, 1)
	assert.Equal(t, files[0].(map[string]any)["path"], "a.go")

	d := call(2, "git_diff_summary", `{"paths":["a.go"]}`)
	assert.Contains(t, d["content"].([]any)[0].(map[string]any)["text"].(string), `"new":"2-3"`)

	bad := call(3, "git_blame", `{"file":"../escape","start":1}`)
	assert.Equal(t, bad["isError"], true)
	unknown := call(4, "git_log", `{"nonsense":1}`)
	assert.Equal(t, unknown["isError"], true, "unknown arguments are refused")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, reports, 4)
	assert.Equal(t, reports[0].Tool, "git_status")
	assert.Equal(t, reports[0].Client, "claude-code")
	assert.True(t, reports[0].ResponseTokens > 0)
	assert.False(t, reports[0].Failed)
	assert.True(t, reports[2].Failed && reports[3].Failed)
	assert.Equal(t, auth, "Bearer csk_test")
	for _, r := range reports {
		raw, _ := json.Marshal(r)
		assert.NotContains(t, string(raw), "a.go", "nothing about the checkout is reported")
	}
}

func TestBridgeLeavesOtherToolsAlone(t *testing.T) {
	b := &bridge{out: &bytes.Buffer{}}
	for _, name := range []string{"search", "get_pr_context", "repo_status", "git_unknown"} {
		msg := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":{}}}`
		var m rpcMessage
		require.NoError(t, json.Unmarshal([]byte(msg), &m))
		assert.False(t, b.answerGit(context.Background(), []byte(msg), m), name+" goes on to the server")
	}
	// A notification (no id) is never answered.
	msg := `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"git_status"}}`
	var m rpcMessage
	require.NoError(t, json.Unmarshal([]byte(msg), &m))
	assert.False(t, b.answerGit(context.Background(), []byte(msg), m))
}

func TestGitToolsOutsideACheckoutSayWhy(t *testing.T) {
	t.Chdir(t.TempDir())
	var out bytes.Buffer
	b := &bridge{out: &out}
	msg := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"git_status","arguments":{}}}`
	var m rpcMessage
	require.NoError(t, json.Unmarshal([]byte(msg), &m))
	require.True(t, b.answerGit(context.Background(), []byte(msg), m))
	assert.Contains(t, out.String(), "git checkout")
	assert.Contains(t, out.String(), `"isError":true`)
}
