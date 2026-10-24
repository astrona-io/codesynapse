package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// gitRepo makes a committed Go module with an origin on github.com.
func gitRepo(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/demo\n\ngo 1.22\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "demo.go"), []byte("package demo\n\n// Synced is in the server's graph.\nfunc Synced() int { return 1 }\n"), 0o600))
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "-A"},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "init"},
		{"remote", "add", "origin", "https://github.com/Acme/demo.git"},
	} {
		_, err := gitx.Run(ctx, dir, args...)
		require.NoError(t, err, strings.Join(args, " "))
	}
	return dir
}

// TestBridgeAnswersFromTheWorkingTree: core tools for this checkout are
// answered locally while it has unsynced changes; everything else, and
// everything while the tree is clean, goes to the server.
func TestBridgeAnswersFromTheWorkingTree(t *testing.T) {
	dir := gitRepo(t)
	var mu sync.Mutex
	var forwarded []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			return
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		body, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(body, &m))
		mu.Lock()
		forwarded = append(forwarded, m.Method+" "+m.Params.Name+" "+string(m.ID))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"from":"server"}}`, m.ID)
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	wt := newWorktree(ctx, filepath.Join(dir), "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.True(t, wt != nil)
	assert.Equal(t, wt.repo, "acme/demo", "named like the server names a github.com clone")

	call := func(lines ...string) map[string]json.RawMessage {
		t.Helper()
		in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" + strings.Join(lines, "\n") + "\n"
		var out bytes.Buffer
		b := &bridge{url: srv.URL, token: "csk_test", session: "chat-1", out: &out, http: srv.Client(), worktree: wt}
		require.NoError(t, b.run(ctx, strings.NewReader(in)))
		got := map[string]json.RawMessage{}
		for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			var m struct {
				ID     json.RawMessage `json:"id"`
				Result json.RawMessage `json:"result"`
			}
			require.NoError(t, json.Unmarshal([]byte(line), &m), line)
			got[string(m.ID)] = m.Result
		}
		return got
	}
	search := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"query":"Edited"}}}`

	got := call(search)
	assert.Equal(t, string(got["2"]), `{"from":"server"}`, "a clean tree is answered by the server")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "demo.go"), []byte("package demo\n\n// Synced is in the server's graph.\nfunc Synced() int { return 1 }\n\n// Edited is not synced yet.\nfunc Edited() int { return 2 }\n"), 0o600))
	mu.Lock()
	forwarded = nil
	mu.Unlock()
	got = call(search,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search","arguments":{"repo":"someone/else","query":"Edited"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"security_report","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"file_outline","arguments":{"repo":"acme/demo","file":"demo.go"}}}`,
	)
	var local struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Structured struct {
			Results []struct {
				Name string `json:"name"`
			} `json:"results"`
		} `json:"structuredContent"`
		Meta map[string]any `json:"_meta"`
	}
	require.NoError(t, json.Unmarshal(got["2"], &local), string(got["2"]))
	assert.Equal(t, local.Meta["codesynapse/source"], "worktree")
	assert.Equal(t, local.Meta["codesynapse/changed_files"], 1.0)
	require.Len(t, local.Structured.Results, 1)
	assert.Equal(t, local.Structured.Results[0].Name, "demo.Edited", "the unsynced function is found")
	assert.Contains(t, local.Content[0].Text, "working tree")
	assert.Contains(t, string(got["5"]), `"name":"demo.Edited"`, "file_outline of this checkout is local too")
	assert.Equal(t, string(got["3"]), `{"from":"server"}`, "another repository goes to the server")
	assert.Equal(t, string(got["4"]), `{"from":"server"}`, "tools that are not core go to the server")
	mu.Lock()
	defer mu.Unlock()
	for _, f := range forwarded {
		assert.False(t, strings.HasSuffix(f, " 2") || strings.HasSuffix(f, " 5"), "answered locally, never sent: "+f)
	}
}

func TestWorktreeLocalCalls(t *testing.T) {
	w := &worktree{repo: "acme/demo"}
	for _, tc := range []struct {
		msg  string
		want bool
	}{
		{`{"params":{"name":"minimal_context","arguments":{"task":"x"}}}`, true},
		{`{"params":{"name":"get_pr_context","arguments":{"repo":"acme/demo"}}}`, true},
		{`{"params":{"name":"repo_status","arguments":{}}}`, false},
		{`{"params":{"name":"search","arguments":{"repo":"acme/other"}}}`, false},
		{`{"params":{"name":"search","arguments":"nope"}}`, false},
	} {
		_, ok := w.local([]byte(tc.msg))
		assert.Equal(t, ok, tc.want, tc.msg)
	}
	var none *worktree
	_, ok := none.local([]byte(`{"params":{"name":"search"}}`))
	assert.False(t, ok, "no working tree: everything is forwarded")
}

// TestBridgePRContextSaysWhatTheChangeIntroduces: get_pr_context without
// arguments analyses the working tree's change against its base and says
// which findings it introduces and fixes.
func TestBridgePRContextSaysWhatTheChangeIntroduces(t *testing.T) {
	ctx := context.Background()
	dir := gitRepo(t)
	write := func(src string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "hash.go"), []byte("package demo\n\nimport (\n\t\"crypto/md5\"\n\t\"crypto/sha256\"\n)\n\n"+src), 0o600))
	}
	write("// Old hashes with MD5.\nfunc Old(b []byte) [16]byte { return md5.Sum(b) }\n\n// Strong hashes with SHA-256.\nfunc Strong(b []byte) [32]byte { return sha256.Sum256(b) }\n")
	for _, args := range [][]string{{"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "hash"}} {
		_, err := gitx.Run(ctx, dir, args...)
		require.NoError(t, err)
	}
	// Old stops using MD5; a new function starts.
	write("// Old hashes with SHA-256 now.\nfunc Old(b []byte) [32]byte { return sha256.Sum256(b) }\n\n// Strong hashes with SHA-256.\nfunc Strong(b []byte) [32]byte { return sha256.Sum256(b) }\n\n// Legacy hashes with MD5.\nfunc Legacy(b []byte) [16]byte { return md5.Sum(b) }\n")

	wt := newWorktree(ctx, dir, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.True(t, wt != nil)
	res, err := wt.answer(ctx, toolCall{Params: struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}{Name: "get_pr_context", Arguments: json.RawMessage(`{}`)}})
	require.NoError(t, err)
	raw, err := json.Marshal(res["structuredContent"])
	require.NoError(t, err)
	var out struct {
		Security struct {
			Introduced map[string]int `json:"introduced"`
			Fixed      map[string]int `json:"fixed"`
			New        []struct {
				Rule  string `json:"rule"`
				Owner string `json:"owner"`
			} `json:"new"`
			Removed []struct {
				Owner string `json:"owner"`
			} `json:"removed"`
			Base string `json:"base"`
		} `json:"security_delta"`
	}
	require.NoError(t, json.Unmarshal(raw, &out), string(raw))
	require.Len(t, out.Security.New, 1, string(raw))
	assert.Equal(t, out.Security.New[0].Rule, "weak-hash")
	assert.Equal(t, out.Security.New[0].Owner, "demo.Legacy")
	require.Len(t, out.Security.Removed, 1)
	assert.Equal(t, out.Security.Removed[0].Owner, "demo.Old")
	assert.True(t, out.Security.Base != "", "names the base commit")
}
