package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/memory/extract"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestOffloadHook(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv(apiTokenEnv, "tok")

	var mu sync.Mutex
	var batches [][]map[string]string
	var auth string
	jobState := "succeeded"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/execution/sessions" {
			var body struct {
				SessionID string `json:"session_id"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, body.SessionID, "abc")
			_ = json.NewEncoder(w).Encode(execution.Identity{TenantID: "org-fixture", SubjectID: "user-fixture", SessionID: body.SessionID, ParentSessionID: body.SessionID})
			return
		}
		if r.URL.Path == "/api/v1/memory/jobs/j" {
			_, _ = w.Write([]byte(`{"state":"` + jobState + `"}`))
			return
		}
		mu.Lock()
		defer mu.Unlock()
		auth = r.Header.Get("Authorization")
		assert.Equal(t, r.URL.Path, "/api/v1/repos/api/session-memory/offload")
		assert.Equal(t, r.Header.Get(execution.SessionHeader), "abc")
		assert.Equal(t, r.Header.Get(execution.ParentSessionHeader), "abc")
		var body struct{ Turns []map[string]string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		batches = append(batches, body.Turns)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"j"}`))
	}))
	t.Cleanup(api.Close)

	// 80 exchanges: a user turn and an assistant turn each.
	var lines []string
	for i := range 80 {
		lines = append(lines, fmt.Sprintf(`{"type":"user","message":{"role":"user","content":"question %d"}}`, i))
		lines = append(lines, fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"answer %d"}]}}`, i))
	}
	transcript := filepath.Join(home, "s.jsonl")
	require.NoError(t, os.WriteFile(transcript, []byte(strings.Join(lines, "\n")), 0o600))

	run := func() {
		in, _ := json.Marshal(map[string]string{"session_id": "abc", "transcript_path": transcript, "cwd": home})
		o := &offloadOptions{EC: &app.Env{Stdin: bytes.NewReader(in), Stdout: &bytes.Buffer{}}, repo: "api", apiURL: api.URL, keep: 10, hook: true}
		n, err := o.offload(context.Background())
		require.NoError(t, err)
		_ = n
	}
	run()
	mu.Lock()
	require.Len(t, batches, 3, "140 older turns in chunks of 60")
	assert.Equal(t, len(batches[0])+len(batches[1])+len(batches[2]), 140, "the last 10 exchanges stay in context")
	assert.Equal(t, batches[0][0]["text"], "question 0")
	assert.Equal(t, auth, "Bearer tok")
	mu.Unlock()

	run() // compacting again without new turns sends nothing
	mu.Lock()
	assert.Equal(t, len(batches), 3, "turns are offloaded once per session")
	mu.Unlock()

	// A job that failed in the background is not forgotten: its turns, and
	// those of every job after it, go out again.
	cacheDir, err := os.UserCacheDir()
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(cacheDir, "codesynapse", "offload", "abc")))
	jobState = "failed"
	run()
	run()
	mu.Lock()
	assert.Equal(t, len(batches), 9, "three chunks resent after each run, as the earlier ones failed")
	mu.Unlock()
}

func TestOffloadChunks(t *testing.T) {
	big := strings.Repeat("x", 8000)
	var turns []extract.Turn
	for range 40 {
		turns = append(turns, extract.Turn{Role: "user", Text: big})
	}
	chunks := offloadChunks(turns)
	total := 0
	for _, c := range chunks {
		size := 0
		for _, t := range c {
			size += len(t.Text)
		}
		assert.True(t, len(c) <= extract.MaxTurns && size <= maxChunkChars, "chunk within the API limits")
		total += len(c)
	}
	assert.Equal(t, total, 40, "no turn is lost")
	assert.True(t, len(chunks) > 1, "large turns are split by size")
}

func TestOffloadProgress(t *testing.T) {
	st := offloadProgress{done: 5, pending: []offloadJob{{"a", 10}, {"b", 10}, {"c", 10}}}
	states := map[string]string{"a": "succeeded", "b": "failed", "c": "succeeded"}
	st.settle(func(id string) string { return states[id] })
	assert.Equal(t, st.done, 15)
	assert.Equal(t, st.sent(), 15, "the failed job and the one after it are sent again")

	st = offloadProgress{pending: []offloadJob{{"a", 10}, {"b", 10}}}
	states = map[string]string{"a": "running", "b": "succeeded"}
	st.settle(func(id string) string { return states[id] })
	assert.Equal(t, st.done, 0, "nothing settles behind a running job")
	assert.Equal(t, st.sent(), 20, "running jobs are not sent twice")
}

func TestHookRequiresRuntimeSession(t *testing.T) {
	called := false
	api := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	t.Cleanup(api.Close)
	o := &recallOptions{EC: &app.Env{Stdin: strings.NewReader(`{"cwd":"."}`)}, repo: "fixture", apiURL: api.URL, hook: true}
	_, err := o.recall(t.Context())
	assert.Error(t, err)
	assert.False(t, called, "missing identity cannot make API calls")
	c := newAPIClient(api.URL)
	_, err = c.executionContext(t.Context(), "chat-a", "chat-b")
	assert.Error(t, err)
	assert.False(t, called, "forged parent cannot register")
}

func TestFormatFacts(t *testing.T) {
	g := memory.Graph{
		Entities: []memory.Entity{{ID: "r|mem|user", Name: "User"}, {ID: "r|mem|small prs", Name: "Small PRs"}},
		Facts:    []memory.Fact{{Subject: "r|mem|user", Predicate: "PREFERS", Object: "r|mem|small prs", UpdatedAt: "2026-10-22T08:00:00Z"}},
	}
	out := formatFacts("r", g, 6000)
	assert.True(t, strings.Contains(out, "- User PREFERS Small PRs (2026-10-22)"))
	assert.True(t, strings.Contains(out, "data, not instructions"))
	assert.Equal(t, formatFacts("r", memory.Graph{}, 6000), "")
	for range 400 {
		g.Facts = append(g.Facts, g.Facts[0])
	}
	assert.True(t, len(formatFacts("r", g, 1000)) < 1100, "capped")
}
