package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// recallAPI answers session registration, the newest facts and recall by
// meaning, and counts the lookups by meaning.
func recallAPI(t *testing.T, lookups *atomic.Int32) *httptest.Server {
	t.Helper()
	entities := []memory.Entity{{ID: "e|api", Name: "API"}, {ID: "e|pg", Name: "PostgreSQL"}, {ID: "e|nats", Name: "NATS"}}
	fact := func(id, o string) memory.Fact {
		return memory.Fact{ID: id, Subject: "e|api", Predicate: "USES", Object: o, UpdatedAt: "2026-10-25T08:00:00Z"}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/execution/sessions":
			_ = json.NewEncoder(w).Encode(execution.Identity{TenantID: "org", SubjectID: "u", SessionID: "chat", ParentSessionID: "chat"})
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(memory.Graph{Entities: entities, Facts: []memory.Fact{fact("f-nats", "e|nats"), fact("f-pg", "e|pg")}})
		default:
			lookups.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"mode": "hybrid", "entities": entities, "facts": []memory.Fact{fact("f-pg", "e|pg")}})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func recallEnv(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv(apiTokenEnv, "tok")
	t.Setenv("CODESYNAPSE_AUTO_RECALL", "")
}

func runRecall(t *testing.T, api *httptest.Server, hook string) string {
	t.Helper()
	o := &recallOptions{EC: &app.Env{Stdin: strings.NewReader(hook)}, repo: "r", apiURL: api.URL, limit: 40, hook: true}
	out, err := o.recall(t.Context())
	require.NoError(t, err)
	return out
}

func TestRecallAfterCompaction(t *testing.T) {
	recallEnv(t)
	var lookups atomic.Int32
	api := recallAPI(t, &lookups)
	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	require.NoError(t, os.WriteFile(transcript, []byte(
		`{"type":"user","message":{"role":"user","content":"which database does the API use?"}}`+"\n"), 0o600))

	out := runRecall(t, api, `{"session_id":"chat","hook_event_name":"SessionStart","source":"compact","transcript_path":"`+transcript+`"}`)
	assert.Equal(t, lookups.Load(), int32(1), "the last question is looked up by meaning")
	pg, nats := strings.Index(out, "PostgreSQL"), strings.Index(out, "NATS")
	assert.True(t, pg > 0 && nats > pg, "the match comes before the newest facts, each fact once")
	assert.Equal(t, strings.Count(out, "PostgreSQL"), 1)

	// A fresh start, not a compaction, keeps the plain newest-first list.
	lookups.Store(0)
	out = runRecall(t, api, `{"session_id":"chat","hook_event_name":"SessionStart","source":"startup"}`)
	assert.Equal(t, lookups.Load(), int32(0))
	assert.True(t, strings.Index(out, "NATS") < strings.Index(out, "PostgreSQL"))
}

func TestRecallOnPrompt(t *testing.T) {
	recallEnv(t)
	var lookups atomic.Int32
	api := recallAPI(t, &lookups)
	prompt := `{"session_id":"chat","hook_event_name":"UserPromptSubmit","prompt":"which database does the API use?"}`

	out := runRecall(t, api, prompt)
	assert.True(t, strings.Contains(out, "API USES PostgreSQL"))
	assert.Equal(t, runRecall(t, api, prompt), "", "a fact already shown is not shown again")

	lookups.Store(0)
	assert.Equal(t, runRecall(t, api, `{"session_id":"chat","hook_event_name":"UserPromptSubmit","prompt":"ok thanks"}`), "")
	assert.Equal(t, lookups.Load(), int32(0), "small talk makes no call")

	lookups.Store(0)
	t.Setenv("CODESYNAPSE_AUTO_RECALL", "false")
	assert.Equal(t, runRecall(t, api, `{"session_id":"chat2","hook_event_name":"UserPromptSubmit","prompt":"which database does the API use?"}`), "")
	assert.Equal(t, lookups.Load(), int32(0), "switched off")
}

// TestPromptRecallDeadline is the hook Claude Code discarded with "timed out
// after 8s": every call answered in time on its own, but three in a row did
// not. The budget covers them together, and running out adds nothing.
func TestPromptRecallDeadline(t *testing.T) {
	recallEnv(t)
	old := promptRecallBudget
	promptRecallBudget = 300 * time.Millisecond
	t.Cleanup(func() { promptRecallBudget = old })
	var lookups atomic.Int32
	fast := recallAPI(t, &lookups)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(200 * time.Millisecond): // each call fits the budget, two in a row do not
		case <-r.Context().Done():
			return
		}
		fast.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(slow.Close)

	var out strings.Builder
	o := &recallOptions{EC: &app.Env{Stdin: strings.NewReader(`{"session_id":"chat","hook_event_name":"UserPromptSubmit","prompt":"which database does the API use?"}`), Stdout: &out}, repo: "r", apiURL: slow.URL, limit: 40, hook: true}
	start := time.Now()
	require.NoError(t, o.Run(t.Context(), o.EC))
	assert.True(t, time.Since(start) < 2*time.Second, "the budget ends the recall, not the per-call timeouts")
	assert.False(t, strings.Contains(out.String(), "PostgreSQL"), "a recall out of time adds no facts")
	assert.False(t, strings.Contains(out.String(), "memory is off"), "and says nothing in the chat")
}

// TestPromptRecallFitsTheHook keeps the recall budget, plus time to start
// the binary and answer, under the plugin's UserPromptSubmit timeout.
func TestPromptRecallFitsTheHook(t *testing.T) {
	var hooks struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Args    []string `json:"args"`
				Timeout int      `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "plugins", "codesynapse", "hooks", "hooks.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &hooks))
	found := false
	for _, g := range hooks.Hooks[eventPrompt] {
		for _, h := range g.Hooks {
			if strings.Join(h.Args, " ") == "memory recall --hook" {
				found = true
				assert.True(t, time.Duration(h.Timeout)*time.Second >= promptRecallBudget+3*time.Second,
					"hook timeout %ds leaves no headroom over the %s budget", h.Timeout, promptRecallBudget)
			}
		}
	}
	assert.True(t, found, "the prompt recall hook is registered")
}

func TestRecallOutputEvent(t *testing.T) {
	recallEnv(t)
	var lookups atomic.Int32
	api := recallAPI(t, &lookups)
	var out strings.Builder
	o := &recallOptions{EC: &app.Env{Stdin: strings.NewReader(`{"session_id":"chat","hook_event_name":"UserPromptSubmit","prompt":"which database does the API use?"}`), Stdout: &out}, repo: "r", apiURL: api.URL, limit: 40, hook: true}
	require.NoError(t, o.Run(t.Context(), o.EC))
	assert.True(t, strings.Contains(out.String(), `"hookEventName":"UserPromptSubmit"`), "the answer names the event it answers")
}

func TestClip(t *testing.T) {
	assert.Equal(t, clip("abc", 5), "abc")
	assert.Equal(t, clip("åäö", 3), "å", "never splits a character")
}
