package llm

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// fakeOllama serves /api/tags and /api/show for models, which a test may
// change, and answers chat with the model it was asked for.
type fakeOllama struct {
	mu     sync.Mutex
	models []ServedModel
	asked  string
}

func (f *fakeOllama) set(models ...ServedModel) {
	f.mu.Lock()
	f.models = models
	f.mu.Unlock()
}

func (f *fakeOllama) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	caps := func(m ServedModel) []string {
		var c []string
		if m.Chat {
			c = append(c, "completion")
		}
		if m.Embedding {
			c = append(c, "embedding")
		}
		if m.Thinking {
			c = append(c, "thinking")
		}
		return c
	}
	switch r.URL.Path {
	case "/api/tags":
		var tags []map[string]any
		for _, m := range f.models {
			tags = append(tags, map[string]any{"name": m.Name, "size": m.Bytes, "capabilities": caps(m), "details": map[string]any{"parameter_size": "1B"}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": tags})
	case "/api/show":
		var in struct{ Model string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		for _, m := range f.models {
			if m.Name == in.Model {
				_ = json.NewEncoder(w).Encode(map[string]any{"model_info": map[string]any{"general.parameter_count": m.ParamsB * 1e9, "arch.context_length": m.Context}})
			}
		}
	case "/v1/chat/completions":
		var in struct{ Model string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.asked = in.Model
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	default:
		http.NotFound(w, r)
	}
}

var (
	big   = ServedModel{Name: "qwen3.8:27b", Chat: true, Thinking: true, ParamsB: 27.8, Context: 262144, Bytes: 18e9}
	small = ServedModel{Name: "qwen3:8b", Chat: true, ParamsB: 8.2, Context: 40960, Bytes: 5e9}
	embed = ServedModel{Name: "nomic-embed-text:latest", Embedding: true, ParamsB: 0.137, Context: 2048}
)

func TestDiscoverOllama(t *testing.T) {
	f := &fakeOllama{}
	f.set(small, big, embed)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	got, err := Discover(t.Context(), srv.URL+"/v1", "")
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, got[1], ServedModel{Name: "qwen3.8:27b", Chat: true, Thinking: true, ParamsB: 27.8, Context: 262144, Bytes: 18e9})
	assert.True(t, got[2].Embedding && !got[2].Chat)
	chat, emb := Best(got)
	assert.Equal(t, chat.Name, "qwen3.8:27b", "the most parameters wins")
	assert.Equal(t, emb.Name, "nomic-embed-text:latest")
}

func TestDiscoverOtherServers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"mlx-community/Qwen3-30B"},{"id":"text-embedding-nomic"}]}`)
	}))
	t.Cleanup(srv.Close)
	got, err := Discover(t.Context(), srv.URL+"/v1", "")
	require.NoError(t, err)
	assert.Equal(t, got, []ServedModel{{Name: "mlx-community/Qwen3-30B", Chat: true}, {Name: "text-embedding-nomic", Embedding: true}})
}

func TestParseParams(t *testing.T) {
	for in, want := range map[string]float64{"27.8B": 27.8, "137M": 0.137, "1.5T": 1500, "": 0, "big": 0} {
		assert.Equal(t, parseParams(in), want, in)
	}
}

// An auto provider follows what its server serves: the strongest chat
// model with its context window, and an embedding model, picked again when
// the server changes.
func TestAutoProviderFollowsTheServer(t *testing.T) {
	f := &fakeOllama{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	var c Config
	require.NoError(t, c.Add(ProviderConfig{Name: "local", BaseURL: srv.URL + "/v1", Model: Auto, EmbeddingModel: Auto}, func(string) string { return "" }))
	r := NewRegistry(c)
	assert.True(t, r.HasAuto())

	_, err := r.Complete(modelContext(t), "local", Request{User: "hi"})
	assert.ErrorIs(t, err, ErrNoServedModel, "nothing picked before the server answered")

	f.set(small, embed)
	require.Len(t, r.Resolve(t.Context()), 1)
	cfg, _ := r.Config("local")
	assert.Equal(t, []any{cfg.Model, cfg.EmbeddingModel, cfg.ContextWindow}, []any{"qwen3:8b", "nomic-embed-text:latest", 40960})
	_, err = r.Complete(modelContext(t), "local", Request{User: "hi"})
	require.NoError(t, err)
	assert.Equal(t, f.asked, "qwen3:8b")

	f.set(small, big, embed) // a bigger model was pulled
	require.Len(t, r.Resolve(t.Context()), 1)
	cfg, _ = r.Config("local")
	assert.Equal(t, cfg.Model, "qwen3.8:27b")
	assert.Equal(t, cfg.ContextWindow, 262144)
	assert.True(t, cfg.MaxInputTokens > 200000, "budgets follow the new window")
	assert.Equal(t, cfg.MaxOutputTokens, 32768)
	_, err = r.Complete(modelContext(t), "local", Request{User: "hi"})
	require.NoError(t, err)
	assert.Equal(t, f.asked, "qwen3.8:27b")
	name, _, err := r.EmbeddingProvider()
	require.NoError(t, err)
	assert.Equal(t, name, "local")

	assert.Len(t, r.Resolve(t.Context()), 0, "no change, nothing reported")
	d, ok := r.Served("local")
	require.True(t, ok)
	assert.Len(t, d.Models, 3)

	srv.Close() // the server went away: the last choice stays
	assert.Len(t, r.Resolve(t.Context()), 0)
	cfg, _ = r.Config("local")
	assert.Equal(t, cfg.Model, "qwen3.8:27b")
	d, _ = r.Served("local")
	assert.True(t, d.Error != "")
}

func TestAutoIsOnlyForLocalServers(t *testing.T) {
	var c Config
	err := c.Add(ProviderConfig{Name: "cloud", BaseURL: "https://api.example.com/v1", Model: Auto}, func(string) string { return "" })
	assert.Error(t, err)
	var a Config
	assert.Error(t, a.Add(ProviderConfig{Name: "claude", Kind: KindAnthropic, Model: Auto}, func(string) string { return "" }))
}

func TestDiscoverProvider(t *testing.T) {
	empty := &fakeOllama{}
	none := httptest.NewServer(empty)
	t.Cleanup(none.Close)
	f := &fakeOllama{}
	f.set(embed, big)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	p, ok := DiscoverProvider(t.Context(), []string{"https://api.example.com/v1", none.URL + "/v1", srv.URL + "/v1"})
	require.True(t, ok, "skips cloud addresses and servers without a chat model")
	assert.Equal(t, p, ProviderConfig{Name: "local", Kind: KindOpenAI, BaseURL: srv.URL + "/v1", Model: Auto, EmbeddingModel: Auto})
	_, ok = DiscoverProvider(t.Context(), []string{none.URL + "/v1"})
	assert.False(t, ok)
}
