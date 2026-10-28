package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/ingest"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestLocalAnswersAreRecordedFirst: the bridge answers a call itself only
// after the server recorded it, with the chat's session, the client and the
// checkout's repository; a refusal sends calls to the server instead.
func TestLocalAnswersAreRecordedFirst(t *testing.T) {
	var mu sync.Mutex
	status := http.StatusNoContent
	var reports []localUsage
	var sessions []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos" {
			_, _ = io.WriteString(w, `{"repos":[{"name":"acme/demo","remote":"https://github.com/Acme/demo.git"}]}`)
			return
		}
		var u localUsage
		require.NoError(t, json.NewDecoder(r.Body).Decode(&u))
		mu.Lock()
		defer mu.Unlock()
		reports = append(reports, u)
		sessions = append(sessions, r.Header.Get(execution.SessionHeader)+"/"+r.Header.Get(execution.ParentSessionHeader))
		w.WriteHeader(status)
		if status != http.StatusNoContent {
			_, _ = fmt.Fprint(w, `{"detail":"refused"}`)
		}
	}))
	t.Cleanup(api.Close)
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()

	ctx, err := execution.With(context.Background(), execution.Identity{TenantID: "org-1", SubjectID: "user-1", SessionID: "chat-1", ParentSessionID: "chat-1"})
	require.NoError(t, err)
	var out bytes.Buffer
	b := &bridge{token: "csk_test", out: &out, gitDir: gitRepo(t), client: "codex", logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		report: apiClient{base: api.URL, token: "csk_test", http: api.Client()}}
	answer := func(id int, name string) bool {
		t.Helper()
		out.Reset()
		msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":{}}}`, id, name)
		var m rpcMessage
		require.NoError(t, json.Unmarshal([]byte(msg), &m))
		ok := b.answerGit(ctx, []byte(msg), m)
		assert.Equal(t, out.Len() > 0, ok, "an answer is written only when the call is answered here")
		return ok
	}
	reported := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(reports)
	}

	require.True(t, answer(1, "git_status"))
	mu.Lock()
	require.Len(t, reports, 1)
	assert.Equal(t, reports[0], localUsage{Tool: "git_status", Client: "codex", Repo: "acme/demo", DurationMs: reports[0].DurationMs, ResponseTokens: reports[0].ResponseTokens})
	assert.Equal(t, sessions[0], "chat-1/chat-1", "the report carries the chat's runtime session")
	mu.Unlock()

	// The server cannot be reached: this call goes to the server, the next is
	// answered here again.
	b.report.base = down.URL
	assert.False(t, answer(2, "git_status"))
	b.report.base = api.URL
	require.True(t, answer(3, "git_status"))

	// A server that does not take reports of a tool (an older one answers
	// 400): that tool goes to the server from now on, others stay local.
	mu.Lock()
	status = http.StatusBadRequest
	mu.Unlock()
	assert.False(t, answer(4, "git_log"))
	n := reported()
	assert.False(t, answer(5, "git_log"))
	assert.Equal(t, reported(), n, "a refused tool is not asked about again")
	mu.Lock()
	status = http.StatusNoContent
	mu.Unlock()
	require.True(t, answer(6, "git_status"))

	// The session was refused, or the audit log could not be written: every
	// call goes to the server from now on.
	for _, refused := range []int{http.StatusForbidden, http.StatusServiceUnavailable} {
		b.localOff = false
		mu.Lock()
		status = refused
		mu.Unlock()
		assert.False(t, answer(7, "git_status"))
		mu.Lock()
		status = http.StatusNoContent
		mu.Unlock()
		n = reported()
		assert.False(t, answer(8, "git_status"))
		assert.False(t, answer(9, "git_blame"))
		assert.Equal(t, reported(), n)
	}
}

// TestWorktreeRemembersFailedBuilds: a tree whose graph could not be built
// is not built again until it changes, and a policy that needs approval is
// not built again until the file changes.
func TestWorktreeRemembersFailedBuilds(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Run("failed build", func(t *testing.T) {
		ctx := context.Background()
		boom := errors.New("boom")
		w := &worktree{root: t.TempDir(), repo: "acme/demo", logger: logger,
			ingest: func(context.Context, ingest.Options) (*model.Graph, error) { return nil, boom }}
		_, err := w.graphFor(ctx, "a")
		assert.True(t, errors.Is(err, boom))
		_, err = w.graphFor(ctx, "a")
		assert.True(t, errors.Is(err, errWorktreeUnavailable))
		assert.Equal(t, w.builds, 1, "the same tree is not built again")
		_, err = w.graphFor(ctx, "b")
		assert.True(t, errors.Is(err, boom))
		assert.Equal(t, w.builds, 2, "a changed tree is")
		w.ingest = func(context.Context, ingest.Options) (*model.Graph, error) { return &model.Graph{}, nil }
		g, err := w.graphFor(ctx, "c")
		require.NoError(t, err)
		assert.True(t, g != nil)
		assert.Equal(t, w.failed, "")
	})
	t.Run("policy needs approval", func(t *testing.T) {
		dir := gitRepo(t)
		ctx := context.Background()
		policy := filepath.Join(dir, ".codesynapse.yaml")
		require.NoError(t, os.WriteFile(policy, []byte("version: 1\nignore:\n  paths: [excluded/**]\n"), 0o600))
		w := &worktree{root: dir, repo: "acme/demo", logger: logger,
			ingest: func(context.Context, ingest.Options) (*model.Graph, error) {
				return nil, fmt.Errorf("policy: %w", ingest.ErrPolicyApproval)
			}}
		_, err := w.graphFor(ctx, "a")
		assert.True(t, errors.Is(err, ingest.ErrPolicyApproval))
		_, err = w.graphFor(ctx, "b")
		assert.True(t, errors.Is(err, errWorktreeUnavailable), "other edits do not lift the refusal")
		assert.Equal(t, w.builds, 1)
		require.NoError(t, os.WriteFile(policy, []byte("version: 1\nignore:\n  paths: [other/**]\n"), 0o600))
		_, err = w.graphFor(ctx, "c")
		assert.True(t, errors.Is(err, ingest.ErrPolicyApproval))
		assert.Equal(t, w.builds, 2, "a changed policy is tried again")
	})
}
