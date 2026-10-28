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
	"strings"
	"sync"
	"testing"

	"github.com/astrona-io/codesynapse/internal/asklocal"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// askCheckout is gitCheckout plus a package directory, an ignored secret,
// a binary and a symlink.
func askCheckout(t *testing.T) string {
	t.Helper()
	dir := gitCheckout(t)
	write := func(p, s string) {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, p), []byte(s), 0o600))
	}
	write("pkg/one.go", "package pkg\n\nfunc One() int { return 1 }\n")
	write("pkg/two.go", "package pkg\n\nfunc Two() int { return 2 }\n")
	write("pkg/blob.bin", "\x00\x01\x02")
	write(".gitignore", ".env\n")
	write(".env", "TOKEN=secret\n")
	write("untracked.go", "package a\n")
	_, err := gitx.Run(context.Background(), dir, "add", "pkg", ".gitignore")
	require.NoError(t, err)
	require.NoError(t, os.Symlink("one.go", filepath.Join(dir, "pkg", "link.go")))
	_, err = gitx.Run(context.Background(), dir, "add", "pkg/link.go")
	require.NoError(t, err)
	return dir
}

func TestReadTracked(t *testing.T) {
	dir := askCheckout(t)
	ctx := context.Background()

	files, skipped, err := readTracked(ctx, dir, []string{"pkg", "a.go:2-99"})
	require.NoError(t, err)
	var got []string
	for _, f := range files {
		got = append(got, f.Path+":"+string(rune('0'+f.Start))+"-"+string(rune('0'+f.End)))
	}
	assert.Equal(t, got, []string{"pkg/one.go:1-3", "pkg/two.go:1-3", "a.go:2-3"}, "the working tree is read; the range is clipped")
	assert.Equal(t, skipped, []string{"pkg/blob.bin", "pkg/link.go"}, "binaries and symlinks are not read")
	assert.Equal(t, files[2].Text, "\nfunc A() {}\n")

	for _, bad := range [][]string{{".env"}, {"untracked.go"}, {"../x"}, {"/etc/passwd"}, {"pkg:1-2"}, {"a.go:9-9"}, {}} {
		_, _, err := readTracked(ctx, dir, bad)
		assert.ErrorIs(t, err, asklocal.ErrInvalid, strings.Join(bad, ","))
	}
}

// The bridge reads the files, has the API answer, returns the answer and
// reports the call like the git tools.
func TestBridgeAnswersAskLocal(t *testing.T) {
	dir := askCheckout(t)
	var mu sync.Mutex
	var asked asklocal.Request
	var reports []localUsage
	var sessionHeader string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/execution/sessions":
			_ = json.NewEncoder(w).Encode(execution.Identity{TenantID: "org", SubjectID: "u", SessionID: "chat-1", ParentSessionID: "chat-1"})
		case "/api/v1/repos/acme%2Fapp/ask-local", "/api/v1/repos/acme/app/ask-local":
			sessionHeader = r.Header.Get(execution.SessionHeader)
			require.NoError(t, json.NewDecoder(r.Body).Decode(&asked))
			_ = json.NewEncoder(w).Encode(asklocal.Answer{Answer: "One returns 1 (pkg/one.go:3).", Model: "qwen", Grounded: true, Files: 1})
		case "/api/v1/usage/local":
			var u localUsage
			require.NoError(t, json.NewDecoder(r.Body).Decode(&u))
			reports = append(reports, u)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	t.Cleanup(api.Close)
	var out bytes.Buffer
	b := &bridge{token: "csk_test", session: "chat-1", out: &out, gitDir: dir, repoName: "acme/app", logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		report: apiClient{base: api.URL, token: "csk_test", http: api.Client()}}
	b.repoOnce.Do(func() {})

	msg := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"ask_local","arguments":{"question":"What does One return?","paths":["pkg/one.go"]}}}`
	var m rpcMessage
	require.NoError(t, json.Unmarshal([]byte(msg), &m))
	require.True(t, b.answerAskLocal(context.Background(), []byte(msg), m))
	var res struct {
		Result map[string]any `json:"result"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &res))
	assert.Equal(t, res.Result["isError"], nil)
	assert.Equal(t, res.Result["structuredContent"].(map[string]any)["answer"], "One returns 1 (pkg/one.go:3).")
	assert.Equal(t, res.Result["_meta"].(map[string]any)["codesynapse/source"], "local-model")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, sessionHeader, "chat-1")
	assert.Equal(t, asked.Question, "What does One return?")
	require.Len(t, asked.Files, 1)
	assert.Equal(t, asked.Files[0].Path, "pkg/one.go")
	require.Len(t, reports, 1)
	assert.Equal(t, reports[0].Tool, asklocal.Tool)
	assert.False(t, reports[0].Failed)

	other := `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"search","arguments":{}}}`
	require.NoError(t, json.Unmarshal([]byte(other), &m))
	assert.False(t, b.answerAskLocal(context.Background(), []byte(other), m), "other tools go to the server")
}

// local_draft sends the target's lines with the context files, clipped to
// the file, and returns the draft without writing anything.
func TestBridgeAnswersLocalDraft(t *testing.T) {
	dir := askCheckout(t)
	var mu sync.Mutex
	var asked asklocal.DraftRequest
	var reports []localUsage
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/execution/sessions":
			_ = json.NewEncoder(w).Encode(execution.Identity{TenantID: "org", SubjectID: "u", SessionID: "chat-1", ParentSessionID: "chat-1"})
		case "/api/v1/repos/acme%2Fapp/local-draft", "/api/v1/repos/acme/app/local-draft":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&asked))
			_ = json.NewEncoder(w).Encode(asklocal.Draft{Warning: asklocal.DraftWarning, Format: asklocal.FormatCode, Code: "func One() int { return 11 }", Model: "qwen", Files: 2})
		case "/api/v1/usage/local":
			var u localUsage
			require.NoError(t, json.NewDecoder(r.Body).Decode(&u))
			reports = append(reports, u)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	t.Cleanup(api.Close)
	var out bytes.Buffer
	b := &bridge{token: "csk_test", session: "chat-1", out: &out, gitDir: dir, repoName: "acme/app", logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		report: apiClient{base: api.URL, token: "csk_test", http: api.Client()}}
	b.repoOnce.Do(func() {})
	call := func(args string) map[string]any {
		t.Helper()
		out.Reset()
		msg := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"local_draft","arguments":` + args + `}}`
		var m rpcMessage
		require.NoError(t, json.Unmarshal([]byte(msg), &m))
		require.True(t, b.answerAskLocal(context.Background(), []byte(msg), m))
		var res struct {
			Result map[string]any `json:"result"`
		}
		require.NoError(t, json.Unmarshal(out.Bytes(), &res))
		return res.Result
	}

	res := call(`{"task":"Make One return 11","paths":["pkg/two.go"],"target":"pkg/one.go:3-9"}`)
	assert.Equal(t, res["isError"], nil)
	assert.Equal(t, res["structuredContent"].(map[string]any)["code"], "func One() int { return 11 }")
	assert.Equal(t, res["structuredContent"].(map[string]any)["warning"], asklocal.DraftWarning)

	for _, bad := range []string{
		`{"task":"x","paths":["pkg/two.go"],"target":"../one.go"}`,
		`{"task":"x","paths":["pkg/two.go"],"target":".env:1-1"}`,
		`{"task":"","paths":["pkg/two.go"]}`,
		`{"task":"x","paths":["pkg/two.go"],"write":true}`,
	} {
		assert.Equal(t, call(bad)["isError"], true, bad)
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, asked.Task, "Make One return 11")
	assert.Equal(t, asked.Target, "pkg/one.go:3-3", "the range is clipped to what was sent")
	require.Len(t, asked.Files, 2)
	assert.Equal(t, []string{asked.Files[0].Path, asked.Files[1].Path}, []string{"pkg/two.go", "pkg/one.go"})
	assert.Equal(t, asked.Files[1].Text, "func One() int { return 1 }\n")
	require.Len(t, reports, 5, "failed calls are reported too")
	assert.Equal(t, reports[0].Tool, asklocal.DraftTool)
	assert.False(t, reports[0].Failed)
	assert.True(t, reports[1].Failed)
}
