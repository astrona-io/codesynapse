package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llm.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
providers:
  - name: local-mlx
    base_url: http://host.docker.internal:9080/v1/
    model: mlx-community/Qwen3.8-27B-4bit
  - name: claude
    kind: anthropic
tasks:
  security-triage: claude
default: local-mlx
`), 0o600))
	env := map[string]string{"ANTHROPIC_API_KEY": "sk-test"}
	cfg, err := Load(path, func(k string) string { return env[k] })
	require.NoError(t, err)
	require.Len(t, cfg.Providers, 2)
	mlx, claude := cfg.Providers[0], cfg.Providers[1]
	assert.Equal(t, mlx.BaseURL, "http://host.docker.internal:9080/v1", "trailing slash trimmed")
	assert.True(t, mlx.Local())
	assert.True(t, mlx.AllowsSource(), "local providers may see source by default")
	assert.Equal(t, claude.Model, "claude-opus-5-5")
	assert.False(t, claude.Local())
	assert.False(t, claude.AllowsSource(), "cloud providers get no source by default")
	assert.True(t, claude.HasKey())

	r := NewRegistry(cfg)
	name, err := r.ProviderFor("security-triage", "")
	require.NoError(t, err)
	assert.Equal(t, name, "claude")
	name, _ = r.ProviderFor("briefing", "")
	assert.Equal(t, name, "local-mlx", "default route")
	_, err = r.ProviderFor("review", "nope")
	assert.Error(t, err)
}

func TestLoadRejects(t *testing.T) {
	for name, yml := range map[string]string{
		"bad kind":      "providers: [{name: a, kind: magic, base_url: http://x}]",
		"no base url":   "providers: [{name: a}]",
		"bad scheme":    "providers: [{name: a, base_url: file:///etc}]",
		"unknown task":  "providers: [{name: a, base_url: http://x}]\ntasks: {hack: a}",
		"unknown route": "providers: [{name: a, base_url: http://x}]\ntasks: {review: b}",
		"unknown field": "providers: [{name: a, base_url: http://x, api_key: secret}]",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "llm.yaml")
			require.NoError(t, os.WriteFile(path, []byte(yml), 0o600))
			_, err := Load(path, func(string) string { return "" })
			assert.Error(t, err)
		})
	}
}

func TestEnvQuickSetupAndMissingFile(t *testing.T) {
	env := map[string]string{"CODESYNAPSE_LLM_BASE_URL": "http://127.0.0.1:9080/v1", "CODESYNAPSE_LLM_MODEL": "m"}
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.yaml"), func(k string) string { return env[k] })
	require.NoError(t, err)
	require.Len(t, cfg.Providers, 1)
	assert.Equal(t, cfg.Default, "env")
}

func TestOpenAICompatible(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"mlx-community/Qwen3.8-27B-4bit"}]}`))
		case "/v1/chat/completions":
			_ = json.NewDecoder(r.Body).Decode(&got)
			_, _ = w.Write([]byte(`{"model":"qwen","choices":[{"message":{"content":"<think>hmm</think>\n{\"ok\":true}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7}}`))
		}
	}))
	defer srv.Close()

	r := NewRegistry(Config{Providers: []ProviderConfig{{Name: "mlx", Kind: KindOpenAI, BaseURL: srv.URL + "/v1", Model: "m", MaxOutputTokens: 100, MaxInputTokens: 100, TimeoutSeconds: 5}}, Default: "mlx"})
	models, err := r.Models(context.Background(), "mlx")
	require.NoError(t, err)
	assert.Equal(t, models, []string{"mlx-community/Qwen3.8-27B-4bit"})

	resp, err := r.Complete(modelContext(t), "mlx", Request{System: "s", User: "token=supersecretvalue1 please, SSN 123-45-6789", MaxTokens: 5000})
	require.NoError(t, err)
	assert.Equal(t, resp.Text, `{"ok":true}`, "think blocks stripped")
	assert.Equal(t, auth, "", "no auth header without a key")
	assert.Equal(t, got["max_tokens"], 100.0, "capped by provider limit")
	msgs := got["messages"].([]any)
	assert.False(t, strings.Contains(msgs[1].(map[string]any)["content"].(string), "supersecretvalue1"), "prompt redacted")
	assert.False(t, strings.Contains(msgs[1].(map[string]any)["content"].(string), "123-45-6789"), "personal data masked")
	u := r.Usage("mlx")
	assert.Equal(t, u.Requests, 1)
	assert.Equal(t, u.InputTokens, 11)
	assert.Equal(t, u.OutputTokens, 7)
}

func TestBudgets(t *testing.T) {
	tests := []struct {
		name            string
		in              ProviderConfig
		wantIn, wantOut int
		wantTimeout     int
	}{
		{"defaults without a window", ProviderConfig{}, 32_000, 8192, 320},
		{"input fills the window", ProviderConfig{ContextWindow: 262_144, MaxOutputTokens: 32_768}, 262_144 - 32_768 - contextMargin, 32_768, 2283},
		{"too large input is clamped", ProviderConfig{ContextWindow: 32_768, MaxInputTokens: 100_000, MaxOutputTokens: 8192}, 32_768 - 8192 - contextMargin, 8192, 300},
		{"output at most half the window", ProviderConfig{ContextWindow: 16_384, MaxOutputTokens: 16_000}, 16_384 - 8192 - contextMargin, 8192, 300},
		{"explicit values win inside the window", ProviderConfig{ContextWindow: 262_144, MaxInputTokens: 50_000, MaxOutputTokens: 4000, TimeoutSeconds: 60}, 50_000, 4000, 60},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := tt.in
			budgets(&p)
			assert.Equal(t, p.MaxInputTokens, tt.wantIn)
			assert.Equal(t, p.MaxOutputTokens, tt.wantOut)
			assert.Equal(t, p.TimeoutSeconds, tt.wantTimeout)
		})
	}
}

func TestEmbeddings(t *testing.T) {
	var got struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		if len(got.Input) == 3 {
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`)) // one short
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"index":1,"embedding":[0,1]},{"index":0,"embedding":[1,0]}]}`))
	}))
	defer srv.Close()
	r := NewRegistry(Config{Providers: []ProviderConfig{
		{Name: "ollama", Kind: KindOpenAI, BaseURL: srv.URL + "/v1", Model: "qwen", EmbeddingModel: "nomic-embed-text", TimeoutSeconds: 5},
		{Name: "cloud", Kind: KindOpenAI, BaseURL: "https://api.example.com/v1", Model: "big", EmbeddingModel: "text-embedding-3-small", TimeoutSeconds: 5},
		{Name: "plain", Kind: KindOpenAI, BaseURL: srv.URL + "/v1", Model: "qwen", TimeoutSeconds: 5},
	}, Default: "ollama"})

	name, cfg, err := r.EmbeddingProvider()
	require.NoError(t, err)
	assert.Equal(t, name, "ollama")
	assert.Equal(t, cfg.EmbeddingModel, "nomic-embed-text")
	vecs, err := r.Embed(modelContext(t), name, []string{"Payments service", "token=supersecretvalue1"})
	require.NoError(t, err)
	assert.Equal(t, vecs, [][]float32{{1, 0}, {0, 1}}, "ordered by index")
	assert.Equal(t, got.Model, "nomic-embed-text")
	assert.False(t, strings.Contains(got.Input[1], "supersecretvalue1"), "inputs redacted")
	_, err = r.Embed(modelContext(t), name, []string{"a", "b", "c"})
	require.Error(t, err, "a missing embedding is an error")

	for _, route := range []string{"cloud", "plain"} {
		require.NoError(t, r.SetRoutes(Routes{Default: "ollama", Tasks: map[string]string{"embeddings": route}}))
		_, _, err = r.EmbeddingProvider()
		assert.True(t, errors.Is(err, ErrNoEmbeddings), route+": cloud or no embedding model is refused")
	}
}
