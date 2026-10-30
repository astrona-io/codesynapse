package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

func TestAIStatusFollowsTheEndpoint(t *testing.T) {
	up := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !up {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen"}]}`))
	}))
	defer srv.Close()
	reg := llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{
		{Name: "local", Kind: llm.KindOpenAI, BaseURL: srv.URL + "/v1", Model: "qwen", MaxOutputTokens: 100, MaxInputTokens: 1000, TimeoutSeconds: 5},
		{Name: "other", Kind: llm.KindOpenAI, BaseURL: srv.URL + "/v1", Model: "missing", MaxOutputTokens: 100, MaxInputTokens: 1000, TimeoutSeconds: 5},
	}, Default: "local"})
	s := &Server{cfg: Config{LLM: reg}}

	got := s.aiStatus(t.Context())
	require.Len(t, got, 2)
	assert.True(t, got[0].Checked && got[0].Online && got[0].ModelAvailable)
	assert.True(t, got[1].Online)
	assert.False(t, got[1].ModelAvailable, "online but the configured model is not served")

	up = false
	assert.True(t, s.aiStatus(t.Context())[0].Online, "a fresh probe is reused")
	s.aiProbe.at = s.aiProbe.at.Add(-aiStatusTTL)
	got = s.aiStatus(t.Context())
	assert.False(t, got[0].Online)
	assert.True(t, got[0].Error != "")
}

func TestLocalModelsSummary(t *testing.T) {
	detail, err := localModelsSummary(llm.LocalCheck{Providers: []llm.ProviderCheck{{Name: "ollama", OK: true}}, Gateway: "ollama", OK: true})
	require.NoError(t, err)
	assert.Equal(t, detail, "1 local provider, gateway on ollama")

	_, err = localModelsSummary(llm.LocalCheck{Providers: []llm.ProviderCheck{{Name: "ollama"}, {Name: "mlx", OK: true}}})
	require.Error(t, err)
	assert.Equal(t, err.Error(), "2 local providers, gateway off: 1 provider fails its check")
}
