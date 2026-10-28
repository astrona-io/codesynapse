package api

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/astrona-io/codesynapse/internal/asklocal"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/usage"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

func localRegistry(url string) *llm.Registry {
	return llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{{Name: "local", Kind: llm.KindOpenAI, BaseURL: url + "/v1", Model: "qwen", MaxOutputTokens: 4000, MaxInputTokens: 30000, TimeoutSeconds: 5}}, Default: "local"})
}

func TestLocalDraftEndpoint(t *testing.T) {
	path := "/api/v1/repos/" + storetest.Repo + "/local-draft"
	body := `{"task":"Make A return 2","target":"pkg/a.go:10-12","files":[{"path":"pkg/a.go","start":10,"end":12,"text":"func A() int {\n\treturn 1\n}\n"}]}`

	none := newRuntimeFixture(t, Config{})
	assert.Equal(t, none.client.do("POST", path, body, nil), 403, "a runtime session is required")
	assert.Equal(t, none.client.do("POST", path, body, nil, runtimeHeaders("thread-a")...), 409, "no model: the agent writes the code itself")

	cloud := llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{{Name: "cloud", Kind: llm.KindOpenAI, BaseURL: "https://api.example.com/v1", Model: "big"}}, Default: "cloud"})
	f := newRuntimeFixture(t, Config{LLM: cloud})
	assert.Equal(t, f.client.do("POST", path, body, nil, runtimeHeaders("thread-a")...), 409, "files never go to a cloud model")

	var system, sent atomic.Value
	var reply atomic.Value
	reply.Store("<think>easy</think>```go\nfunc A() int {\n\treturn 2\n}\n```\nNOTES: replaces pkg/a.go:10-12; see b.go:3.")
	model := chatModel(t, func(sys, user string) string {
		system.Store(sys)
		sent.Store(user)
		return reply.Load().(string)
	})
	f = newRuntimeFixture(t, Config{LLM: localRegistry(model.URL)})
	assert.Equal(t, f.client.do("POST", path, `{"task":"","files":[]}`, nil, runtimeHeaders("thread-a")...), 400)
	assert.Equal(t, f.client.do("POST", path, strings.Replace(body, `pkg/a.go:10-12`, `../a.go`, 1), nil, runtimeHeaders("thread-a")...), 400)

	var d asklocal.Draft
	require.Equal(t, f.client.do("POST", path, body, &d, runtimeHeaders("thread-a")...), 200)
	assert.Equal(t, d.Warning, asklocal.DraftWarning)
	assert.Equal(t, d.Format, asklocal.FormatCode)
	assert.Equal(t, d.Language, "go")
	assert.Equal(t, d.Code, "func A() int {\n\treturn 2\n}")
	assert.Equal(t, d.Target, "pkg/a.go")
	require.NotNil(t, d.Replaces)
	assert.Equal(t, *d.Replaces, asklocal.Citation{Path: "pkg/a.go", Start: 10, End: 12})
	assert.Equal(t, d.Notes, "replaces pkg/a.go:10-12; see b.go:3.")
	assert.Equal(t, d.Citations, []asklocal.Citation{{Path: "pkg/a.go", Start: 10, End: 12}})
	assert.Equal(t, d.Unverified, 1)
	assert.Len(t, d.Problems, 0, "problems: %v", d.Problems)
	assert.Equal(t, []int{d.Files, d.Lines}, []int{1, 3})
	assert.Equal(t, system.Load().(string), draftSystem, "the system prompt stays constant")
	assert.Contains(t, sent.Load().(string), "Replace lines 10-12 of pkg/a.go")
	assert.Contains(t, sent.Load().(string), "11| \treturn 1\n")

	buckets, err := f.store.Usage(t.Context(), "")
	require.NoError(t, err)
	counted := map[string]int{}
	for _, b := range buckets {
		counted[b.Tool] += b.Calls
	}
	assert.Equal(t, counted[usage.LocalDrafts], 1)
	assert.True(t, counted[usage.LocalRead] > 0, "what the local model read is counted")
	assert.Equal(t, counted[usage.LocalAvoided], 0, "a draft claims no tokens avoided")

	for _, tc := range []struct {
		name, reply, format, problem string
	}{
		{name: "bad go", reply: "```go\nfunc A() int {\n\treturn 2 +\n}\n```", format: asklocal.FormatCode, problem: "go syntax: line 3"},
		{name: "diff that matches", reply: "```diff\n--- a/pkg/a.go\n+++ b/pkg/a.go\n@@ -11 +11 @@\n-\treturn 1\n+\treturn 2\n```", format: asklocal.FormatDiff},
		{name: "diff that does not", reply: "```diff\n--- a/pkg/a.go\n+++ b/pkg/a.go\n@@ -11 +11 @@\n-\treturn 7\n+\treturn 2\n```", format: asklocal.FormatDiff, problem: "pkg/a.go:11 does not match the file"},
		{name: "refusal", reply: "CANNOT DRAFT: what should A return otherwise?", format: asklocal.FormatCode, problem: "wrote no draft"},
		{name: "secret", reply: "```go\nconst key = \"AKIAIOSFODNN7EXAMPLE\"\n```", format: asklocal.FormatCode, problem: "redacted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply.Store(tc.reply)
			var d asklocal.Draft
			require.Equal(t, f.client.do("POST", path, body, &d, runtimeHeaders("thread-a")...), 200)
			assert.Equal(t, d.Format, tc.format)
			assert.Equal(t, d.Warning, asklocal.DraftWarning)
			if tc.problem == "" {
				assert.Len(t, d.Problems, 0, "problems: %v", d.Problems)
				return
			}
			require.NotEmpty(t, d.Problems)
			assert.Contains(t, strings.Join(d.Problems, "\n"), tc.problem)
		})
	}
	assert.NotContains(t, d.Code, "AKIA")
}

func TestLocalDraftReportAndTaskGuard(t *testing.T) {
	assert.NoError(t, localUsageReport{Tool: asklocal.DraftTool, Client: "claude-code"}.validate())

	model := chatModel(t, func(_, _ string) string { return "OK" })
	f := newRuntimeFixture(t, Config{LLM: localRegistry(model.URL)})
	assert.Equal(t, f.client.do("POST", "/api/v1/repos/"+storetest.Repo+"/ai/local-draft", "", nil, runtimeHeaders("thread-a")...), 400,
		"local-draft runs only for agents through the bridge")
}
