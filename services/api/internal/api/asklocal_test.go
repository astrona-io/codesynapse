package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/astrona-io/codesynapse/internal/asklocal"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/usage"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

// chatModel answers like an OpenAI-compatible server; reply sees the system
// and user messages.
func chatModel(t *testing.T, reply func(system, user string) string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct{ Role, Content string }
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		system, user := "", ""
		for _, m := range req.Messages {
			if m.Role == "system" {
				system = m.Content
			} else {
				user = m.Content
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "qwen", "usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 10},
			"choices": []any{map[string]any{"message": map[string]any{"content": reply(system, user)}}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAskLocalEndpoint(t *testing.T) {
	path := "/api/v1/repos/" + storetest.Repo + "/ask-local"
	body := `{"question":"Where are routes added?","files":[{"path":"internal/api/api.go","start":10,"end":12,"text":"a\nb\nc\n"}]}`

	none := newRuntimeFixture(t, Config{})
	assert.Equal(t, none.client.do("POST", path, body, nil), 403, "a runtime session is required")
	assert.Equal(t, none.client.do("POST", path, body, nil, runtimeHeaders("thread-a")...), 409, "no model: the agent reads the files itself")

	cloud := llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{{Name: "cloud", Kind: llm.KindOpenAI, BaseURL: "https://api.example.com/v1", Model: "big"}}, Default: "cloud"})
	f := newRuntimeFixture(t, Config{LLM: cloud})
	assert.Equal(t, f.client.do("POST", path, body, nil, runtimeHeaders("thread-a")...), 409, "files never go to a cloud model")

	var sent atomic.Value
	model := chatModel(t, func(_, user string) string {
		sent.Store(user)
		return "<think>hmm</think>Routes are added at internal/api/api.go:11 and in other.go:4."
	})
	local := llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{{Name: "local", Kind: llm.KindOpenAI, BaseURL: model.URL + "/v1", Model: "qwen", MaxOutputTokens: 4000, MaxInputTokens: 30000, TimeoutSeconds: 5}}, Default: "local"})
	f = newRuntimeFixture(t, Config{LLM: local})
	assert.Equal(t, f.client.do("POST", path, `{"question":"","files":[]}`, nil, runtimeHeaders("thread-a")...), 400)
	var ans asklocal.Answer
	require.Equal(t, f.client.do("POST", path, body, &ans, runtimeHeaders("thread-a")...), 200)
	assert.Equal(t, ans.Answer, "Routes are added at internal/api/api.go:11 and in other.go:4.", "thinking is dropped")
	assert.Equal(t, ans.Citations, []asklocal.Citation{{Path: "internal/api/api.go", Start: 11, End: 11}})
	assert.Equal(t, ans.Unverified, 1)
	assert.True(t, ans.Grounded)
	assert.Contains(t, ans.Note, "citations were not among the lines")
	assert.Equal(t, []int{ans.Files, ans.Lines, ans.Batches}, []int{1, 3, 1})
	assert.Contains(t, sent.Load().(string), "11| b\n", "lines are numbered for citing")

	buckets, err := f.store.Usage(t.Context(), "")
	require.NoError(t, err)
	counted := map[string]int{}
	for _, b := range buckets {
		counted[b.Tool] += b.Calls
	}
	assert.True(t, counted[usage.LocalRead] > 0, "what the local model read is counted")
}

// Files larger than one request are read in parts and the notes merged.
func TestAskLocalBatches(t *testing.T) {
	var parts, merges atomic.Int32
	model := chatModel(t, func(system, user string) string {
		switch {
		case strings.Contains(system, "one part"):
			parts.Add(1)
			if strings.Contains(user, "=== b.go") {
				return "NONE"
			}
			return "Handled at a.go:2."
		case strings.Contains(system, "merge"):
			merges.Add(1)
			assert.NotContains(t, user, "NONE", "empty parts are left out")
			return "It is handled at a.go:2."
		}
		t.Errorf("unexpected single request")
		return ""
	})
	reg := llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{{Name: "local", Kind: llm.KindOpenAI, BaseURL: model.URL + "/v1", Model: "qwen", MaxOutputTokens: 200, MaxInputTokens: 2500, TimeoutSeconds: 5}}, Default: "local"})
	line := strings.Repeat("x", 60) + "\n"
	req := asklocal.Request{Question: "Where is it handled?", Files: []asklocal.File{
		{Path: "a.go", Start: 1, End: 20, Text: strings.Repeat(line, 20)},
		{Path: "b.go", Start: 1, End: 20, Text: strings.Repeat(line, 20)},
	}}
	ctx, err := execution.With(t.Context(), fixedExecution())
	require.NoError(t, err)
	ans, err := askLocal(ctx, reg, req)
	require.NoError(t, err)
	assert.Equal(t, ans.Answer, "It is handled at a.go:2.")
	assert.True(t, parts.Load() >= 2)
	assert.Equal(t, merges.Load(), int32(1))
	assert.Equal(t, ans.Batches, int(parts.Load())+1)
	assert.Equal(t, ans.Citations, []asklocal.Citation{{Path: "a.go", Start: 2, End: 2}})
}

func TestPackSplitsWhatDoesNotFit(t *testing.T) {
	file := func(p string, lines int) asklocal.File {
		return asklocal.File{Path: p, Start: 1, End: lines, Text: strings.Repeat("l\n", lines)}
	}
	size := func(fs []asklocal.File) int {
		n := 0
		for _, f := range fs {
			n += f.End - f.Start + 1
		}
		return n
	}
	fitsIn := func(limit int) func([]asklocal.File) (bool, error) {
		return func(fs []asklocal.File) (bool, error) { return size(fs) <= limit, nil }
	}
	batches, err := pack([]asklocal.File{file("a", 3), file("b", 3), file("c", 10)}, fitsIn(6))
	require.NoError(t, err)
	require.Len(t, batches, 3)
	assert.Equal(t, size(batches[0]), 6, "a and b share a batch")
	assert.Equal(t, batches[1][0], asklocal.File{Path: "c", Start: 1, End: 5, Text: strings.Repeat("l\n", 5)}, "c is halved")
	assert.Equal(t, batches[2][0].Start, 6)

	_, err = pack([]asklocal.File{file("a", 1)}, fitsIn(0))
	assert.ErrorIs(t, err, llm.ErrInputBudget)
	_, err = pack([]asklocal.File{file("a", 100)}, fitsIn(1))
	assert.ErrorIs(t, err, llm.ErrInputBudget, "too many batches")
}

// ask_local is reported like the git tools, and its task cannot be queued
// as a review job.
func TestAskLocalReportAndTaskGuard(t *testing.T) {
	assert.NoError(t, localUsageReport{Tool: asklocal.Tool, Client: "claude-code"}.validate())
	assert.Error(t, localUsageReport{Tool: "ask_cloud", Client: "claude-code"}.validate())

	model := chatModel(t, func(_, _ string) string { return "OK" })
	reg := llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{{Name: "local", Kind: llm.KindOpenAI, BaseURL: model.URL + "/v1", Model: "qwen", TimeoutSeconds: 5}}, Default: "local"})
	f := newRuntimeFixture(t, Config{LLM: reg})
	assert.Equal(t, f.client.do("POST", "/api/v1/repos/"+storetest.Repo+"/ai/local-answer", "", nil, runtimeHeaders("thread-a")...), 400)
}

// The pre-brief's plan names line ranges from the graph alone.
func TestBriefPlan(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	path := "/api/v1/repos/" + storetest.Repo + "/brief-plan"
	var plan BriefPlan
	require.Equal(t, f.client.do("POST", path, `{"prompt":"Make Handler validate its input before it runs"}`, &plan, runtimeHeaders("thread-a")...), 200)
	require.True(t, len(plan.Ranges) > 0)
	assert.Contains(t, strings.Join(plan.Ranges, ","), "app/app.go:")
	assert.True(t, len(plan.Ranges) <= maxBriefRanges)
	assert.Equal(t, f.client.do("POST", path, `{"prompt":"  "}`, nil, runtimeHeaders("thread-a")...), 400)
}

// In brief mode the local model briefs instead of answering, and the brief
// is counted.
func TestAskLocalBriefMode(t *testing.T) {
	model := chatModel(t, func(system, user string) string {
		if !strings.Contains(system, "Brief it") || !strings.Contains(user, "The developer's request: add a flag") {
			return "wrong prompt"
		}
		return "**Where** Handler at internal/api/api.go:11."
	})
	reg := llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{{Name: "local", Kind: llm.KindOpenAI, BaseURL: model.URL + "/v1", Model: "qwen", MaxOutputTokens: 4000, MaxInputTokens: 30000, TimeoutSeconds: 5}}, Default: "local"})
	f := newRuntimeFixture(t, Config{LLM: reg})
	path := "/api/v1/repos/" + storetest.Repo + "/ask-local"
	body := `{"question":"add a flag","mode":"brief","files":[{"path":"internal/api/api.go","start":10,"end":12,"text":"a\nb\nc\n"}]}`
	var ans asklocal.Answer
	require.Equal(t, f.client.do("POST", path, body, &ans, runtimeHeaders("thread-a")...), 200)
	assert.Equal(t, ans.Answer, "**Where** Handler at internal/api/api.go:11.")
	assert.True(t, ans.Grounded)
	buckets, err := f.store.Usage(t.Context(), "")
	require.NoError(t, err)
	briefs := 0
	for _, b := range buckets {
		if b.Tool == usage.LocalBriefs {
			briefs += b.Calls
		}
	}
	assert.Equal(t, briefs, 1)
	assert.Equal(t, f.client.do("POST", path, strings.Replace(body, `"brief"`, `"essay"`, 1), nil, runtimeHeaders("thread-a")...), 400)
}
