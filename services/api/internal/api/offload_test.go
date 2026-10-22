package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agentmem "github.com/parisnakitakejser/codesynapse/internal/memory"
	"github.com/parisnakitakejser/codesynapse/internal/store/storetest"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
	"github.com/parisnakitakejser/codesynapse/services/api/internal/llm"
	"github.com/parisnakitakejser/codesynapse/services/api/internal/offload"
)

func TestOffloadEndpoints(t *testing.T) {
	turns := `{"turns":[{"role":"user","text":"From now on the payments service is owned by Team Blue."}],"source":"claude-code"}`
	path := "/api/v1/repos/" + storetest.Repo + "/session-memory/offload"
	noModel := newRuntimeFixture(t, Config{})
	assert.Equal(t, noModel.client.do("POST", path, turns, nil), 403)
	assert.Equal(t, noModel.client.do("POST", path, turns, nil, runtimeHeaders("thread-a")...), 409)
	assert.Equal(t, noModel.client.do("POST", path, `{"turns":[]}`, nil, runtimeHeaders("thread-a")...), 400)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "qwen", "choices": []any{map[string]any{"message": map[string]any{"content": `{"triples":[{"subject":"Payments service","predicate":"OWNED_BY","object":"Team Blue","confidence":0.9}]}`}}}})
	}))
	t.Cleanup(model.Close)
	reg := llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{{Name: "local", Kind: llm.KindOpenAI, BaseURL: model.URL + "/v1", Model: "qwen", MaxOutputTokens: 4000, MaxInputTokens: 30000, TimeoutSeconds: 5}}, Default: "local"})
	f := newRuntimeFixture(t, Config{LLM: reg})
	var job offload.Job
	require.Equal(t, f.client.do("POST", path, turns, &job, runtimeHeaders("thread-a")...), 202)
	assert.Equal(t, job.Execution, f.identity)
	deadline := time.Now().Add(5 * time.Second)
	for job.State != offload.Succeeded && job.State != offload.Failed && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		require.Equal(t, f.client.do("GET", "/api/v1/memory/jobs/"+job.ID, "", &job, runtimeHeaders("thread-a")...), 200)
	}
	assert.Equal(t, job.State, offload.Succeeded, job.Error)
	assert.Equal(t, job.Saved, 1)
	var g agentmem.Graph
	require.Equal(t, f.client.do("GET", "/api/v1/repos/"+storetest.Repo+"/session-memory", "", &g, runtimeHeaders("thread-a")...), 200)
	require.Len(t, g.Facts, 1)
	assert.Equal(t, g.Facts[0].OriginSessionID, "thread-a")
	require.Equal(t, f.client.do("POST", "/api/v1/execution/sessions", `{"session_id":"thread-b"}`, nil), 201)
	assert.Equal(t, f.client.do("GET", "/api/v1/memory/jobs/"+job.ID, "", nil, runtimeHeaders("thread-b")...), 404)
	var jobs struct{ Jobs []offload.Job }
	require.Equal(t, f.client.do("GET", "/api/v1/memory/jobs", "", &jobs, runtimeHeaders("thread-b")...), 200)
	assert.Len(t, jobs.Jobs, 0)
}

func TestRecallAndStatusEndpoints(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	ns := agentmem.Namespace(f.org.ID, storetest.Repo)
	b, err := agentmem.Prepare(ns, []agentmem.Triple{{Subject: "Payments", Predicate: "OWNED_BY", Object: "Team Blue"}}, "a", time.Now())
	require.NoError(t, err)
	require.NoError(t, f.store.UpsertMemory(t.Context(), b))
	path := "/api/v1/repos/" + storetest.Repo
	var res struct {
		Skipped string
		Mode    string
		Facts   []agentmem.Fact
	}
	require.Equal(t, f.client.do("POST", path+"/memory/recall", `{"query":"who owns payments","depth":1}`, &res, runtimeHeaders("thread-a")...), 200)
	require.Len(t, res.Facts, 1)
	require.Equal(t, f.client.do("POST", path+"/session-memory/recall", `{"query":"who owns payments"}`, &res, runtimeHeaders("thread-a")...), 200)
	assert.Len(t, res.Facts, 0, "session recall never falls back to durable knowledge")
	require.Equal(t, f.client.do("POST", path+"/session-memory/recall", `{"query":"thanks!"}`, &res, runtimeHeaders("thread-a")...), 200)
	assert.Equal(t, res.Skipped, "small talk")
	assert.Equal(t, f.client.do("POST", path+"/session-memory/recall", `{"query":"`+strings.Repeat("x", 2001)+`"}`, nil, runtimeHeaders("thread-a")...), 400)
	assert.Equal(t, f.client.do("POST", path+"/memory/offload", `{"turns":[]}`, nil, runtimeHeaders("thread-a")...), 410)
	var status map[string]any
	require.Equal(t, f.client.do("GET", "/api/v1/memory/status", "", &status), 200)
	assert.Equal(t, status["extraction"], false)
}
