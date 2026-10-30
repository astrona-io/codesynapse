package api

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/astrona-io/codesynapse/internal/fetchsource"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/usage"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

func TestSourceExtractEndpoint(t *testing.T) {
	path := "/api/v1/repos/" + storetest.Repo + "/source-extract"
	page := "# Client\n\nUse `NewClient` to connect.\n\n```go\nc, err := NewClient(ctx, addr)\n```\n\n# Other\n\nUnrelated.\n"
	raw, err := json.Marshal(fetchsource.ExtractRequest{Question: "how do I connect", Text: page})
	require.NoError(t, err)
	body := string(raw)

	none := newRuntimeFixture(t, Config{})
	assert.Equal(t, none.client.do("POST", path, body, nil), 403, "a runtime session is required")
	assert.Equal(t, none.client.do("POST", path, body, nil, runtimeHeaders("thread-a")...), 409, "no model: tier 1 stands")

	cloud := llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{{Name: "cloud", Kind: llm.KindOpenAI, BaseURL: "https://api.example.com/v1", Model: "big"}}, Default: "cloud"})
	f := newRuntimeFixture(t, Config{LLM: cloud})
	assert.Equal(t, f.client.do("POST", path, body, nil, runtimeHeaders("thread-a")...), 409, "a page and question never go to a cloud model")

	var system, sent, reply atomic.Value
	reply.Store("<think>easy</think># Client\n\nUse `NewClient`:\n\n```go\nc, err := NewClient(ctx, addr)\n```\n\nOr `Dial(addr)`.")
	model := chatModel(t, func(sys, user string) string {
		system.Store(sys)
		sent.Store(user)
		return reply.Load().(string)
	})
	f = newRuntimeFixture(t, Config{LLM: localRegistry(model.URL)})
	assert.Equal(t, f.client.do("POST", path, `{"question":"","text":"x"}`, nil, runtimeHeaders("thread-a")...), 400)
	assert.Equal(t, f.client.do("POST", path, `{"question":"q","text":"x","url":"https://example.com"}`, nil, runtimeHeaders("thread-a")...), 400, "unknown fields are refused")

	var ex fetchsource.Extract
	require.Equal(t, f.client.do("POST", path, body, &ex, runtimeHeaders("thread-a")...), 200)
	assert.True(t, ex.Found)
	assert.NotContains(t, ex.Text, "think")
	assert.Contains(t, ex.Text, "```go\nc, err := NewClient(ctx, addr)\n```")
	assert.NotContains(t, ex.Text, "Dial", "code the page lacks is dropped")
	assert.Equal(t, ex.Dropped, 1)
	require.Len(t, ex.Problems, 1)
	assert.Equal(t, system.Load().(string), extractSystem, "the system prompt stays constant")
	assert.Contains(t, sent.Load().(string), "Question: how do I connect")
	assert.Contains(t, sent.Load().(string), "Unrelated.")

	buckets, err := f.store.Usage(t.Context(), "")
	require.NoError(t, err)
	counted := map[string]int{}
	for _, b := range buckets {
		counted[b.Tool] += b.Calls
	}
	assert.True(t, counted[usage.LocalRead] > 0, "what the local model read is counted")
	assert.Equal(t, counted[usage.LocalAvoided], 0)

	for _, tc := range []struct {
		name, reply string
		found       bool
		problem     string
	}{
		{name: "not in the page", reply: "NOT IN THE PAGE"},
		{name: "only thinking", reply: "<think>hmm</think>"},
		{name: "secret", reply: "Set key AKIAIOSFODNN7EXAMPLE first.", found: true, problem: "redacted"},
		{name: "too long", reply: strings.Repeat("word ", fetchsource.MaxExtract), found: true, problem: "cut at"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply.Store(tc.reply)
			var ex fetchsource.Extract
			require.Equal(t, f.client.do("POST", path, body, &ex, runtimeHeaders("thread-a")...), 200)
			assert.Equal(t, ex.Found, tc.found)
			assert.NotContains(t, ex.Text, "AKIA")
			assert.True(t, len([]rune(ex.Text)) <= fetchsource.MaxExtract+2)
			if tc.problem != "" {
				assert.Contains(t, strings.Join(ex.Problems, "\n"), tc.problem)
			}
		})
	}

	assert.Equal(t, f.client.do("POST", "/api/v1/repos/"+storetest.Repo+"/ai/source-extract", "", nil, runtimeHeaders("thread-a")...), 400,
		"source-extract runs only for agents through the bridge")
}
