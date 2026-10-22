package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agentmem "github.com/parisnakitakejser/codesynapse/internal/memory"
	"github.com/parisnakitakejser/codesynapse/internal/store/memory"
	"github.com/parisnakitakejser/codesynapse/internal/store/storetest"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
	"github.com/parisnakitakejser/codesynapse/services/api/internal/llm"
	"github.com/parisnakitakejser/codesynapse/services/api/internal/offload"
)

func postJSON(t *testing.T, ts *httptest.Server, path, body string, out any) int {
	t.Helper()
	res, err := http.Post(ts.URL+path, "application/json", strings.NewReader(body)) //nolint:noctx // test
	require.NoError(t, err)
	defer res.Body.Close() //nolint:errcheck // test
	if out != nil {
		_ = json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode
}

func TestOffloadEndpoints(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	turns := `{"turns":[{"role":"user","text":"From now on the payments service is owned by Team Blue."}],"source":"claude-code"}`
	path := "/api/v1/repos/" + storetest.Repo + "/memory/offload"

	noModel := httptest.NewServer(New(ctx, Config{}, st, logger))
	t.Cleanup(noModel.Close)
	assert.Equal(t, postJSON(t, noModel, "/api/v1/repos/nope/memory/offload", turns, nil), 404)
	assert.Equal(t, postJSON(t, noModel, path, turns, nil), 409, "no local model")
	assert.Equal(t, postJSON(t, noModel, path, `{"turns":[]}`, nil), 400)

	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"model":"qwen","choices":[{"message":{"content":"{\"triples\":[{\"subject\":\"Payments service\",\"predicate\":\"OWNED_BY\",\"object\":\"Team Blue\",\"confidence\":0.9}]}"}}]}`))
	}))
	t.Cleanup(model.Close)
	reg := llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{{Name: "local", Kind: llm.KindOpenAI, BaseURL: model.URL + "/v1", Model: "qwen",
		MaxOutputTokens: 4000, MaxInputTokens: 30000, TimeoutSeconds: 5}}, Default: "local"})
	ts := httptest.NewServer(New(ctx, Config{LLM: reg}, st, logger))
	t.Cleanup(ts.Close)
	var job offload.Job
	require.Equal(t, postJSON(t, ts, path, turns, &job), 202)
	assert.Equal(t, job.State, offload.Queued)
	deadline := time.Now().Add(5 * time.Second)
	for job.State != offload.Succeeded && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		require.Equal(t, get(t, ts, "/api/v1/memory/jobs/"+job.ID, &job), 200)
	}
	assert.Equal(t, job.State, offload.Succeeded, job.Error)
	assert.Equal(t, job.Saved, 1)
	var list struct {
		Jobs      []offload.Job
		Available bool
	}
	require.Equal(t, get(t, ts, "/api/v1/memory/jobs?repo="+storetest.Repo, &list), 200)
	assert.True(t, list.Available)
	assert.Equal(t, len(list.Jobs), 1)
	assert.Equal(t, get(t, ts, "/api/v1/memory/jobs/nope", nil), 404)
}

func TestRecallAndStatusEndpoints(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	b, err := agentmem.Prepare(storetest.Repo, []agentmem.Triple{{Subject: "Payments", Predicate: "OWNED_BY", Object: "Team Blue"}}, "a", time.Now())
	require.NoError(t, err)
	require.NoError(t, st.UpsertMemory(ctx, b))
	ts := httptest.NewServer(New(ctx, Config{}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)
	path := "/api/v1/repos/" + storetest.Repo + "/memory/recall"

	var res struct {
		Skipped string
		Mode    string
		Facts   []agentmem.Fact
	}
	require.Equal(t, postJSON(t, ts, path, `{"query":"who owns payments","depth":1}`, &res), 200)
	assert.Equal(t, res.Mode, "names", "no embedding model configured")
	require.Len(t, res.Facts, 1)
	require.Equal(t, postJSON(t, ts, path, `{"query":"thanks!"}`, &res), 200)
	assert.Equal(t, res.Skipped, "small talk")
	assert.Equal(t, postJSON(t, ts, path, `{"query":"`+strings.Repeat("x", 2001)+`"}`, nil), 400)

	var status map[string]any
	require.Equal(t, get(t, ts, "/api/v1/memory/status", &status), 200)
	assert.Equal(t, status["extraction"], false)
	assert.Equal(t, status["embeddings"], false)
}
