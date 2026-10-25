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
