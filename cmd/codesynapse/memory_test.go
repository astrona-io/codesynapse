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

	"github.com/parisnakitakejser/codesynapse/internal/app"
	"github.com/parisnakitakejser/codesynapse/internal/memory"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
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
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		auth = r.Header.Get("Authorization")
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
