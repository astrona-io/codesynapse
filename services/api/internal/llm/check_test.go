package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// checkServer is an Ollama (or, with ollama false, a plain OpenAI-compatible
// server) that records every request body so a test can see nothing but
// model names was sent.
type checkServer struct {
	ollama  bool
	models  []string
	caps    map[string][]string
	trained int    // model_info context_length
	params  string // Modelfile parameters
	loaded  int    // /api/ps context_length; 0 means not loaded
	mu      sync.Mutex
	bodies  []string
}

func (s *checkServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.bodies = append(s.bodies, r.URL.Path+" "+string(body))
	s.mu.Unlock()
	if !s.ollama && strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}
	switch r.URL.Path {
	case "/v1/models":
		var data []map[string]string
		for _, m := range s.models {
			data = append(data, map[string]string{"id": m})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	case "/api/version":
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "0.12.6"})
	case "/api/show":
		var in struct{ Model string }
		_ = json.Unmarshal(body, &in)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"capabilities": s.caps[in.Model],
			"model_info":   map[string]any{"qwen3.context_length": s.trained},
			"parameters":   s.params,
		})
	case "/api/ps":
		var ps []map[string]any
		if s.loaded > 0 {
			ps = append(ps, map[string]any{"name": "qwen3.8:27b", "model": "qwen3.8:27b", "context_length": s.loaded})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": ps})
	default:
		http.NotFound(w, r)
	}
}

func checkRegistry(url string, gateway bool, embed string) *Registry {
	return NewRegistry(Config{Providers: []ProviderConfig{
		{Name: "ollama", Kind: KindOpenAI, BaseURL: url + "/v1", Model: "qwen3.8:27b", EmbeddingModel: embed,
			ContextWindow: 262144, Gateway: gateway, TimeoutSeconds: 5},
		{Name: "cloud", Kind: KindOpenAI, BaseURL: "https://api.example.com/v1", Model: "big", TimeoutSeconds: 5},
	}, Default: "ollama", Tasks: map[string]string{"review": "cloud"}})
}

func TestCheckLocal(t *testing.T) {
	chat := map[string][]string{"qwen3.8:27b": {"completion", "thinking"}, "nomic-embed-text:latest": {"embedding"}}
	models := []string{"qwen3.8:27b", "nomic-embed-text:latest"}
	tests := map[string]struct {
		srv      *checkServer
		gateway  bool
		embed    string
		ok       bool
		problems []string // substrings, in order
		warnings []string
		loaded   int
	}{
		"loaded with the full context": {
			srv:     &checkServer{ollama: true, models: models, caps: chat, trained: 262144, loaded: 262144},
			gateway: true, embed: "nomic-embed-text", ok: true, loaded: 262144,
		},
		"loaded with a smaller context": {
			srv:     &checkServer{ollama: true, models: models, caps: chat, trained: 262144, loaded: 4096},
			gateway: true, embed: "nomic-embed-text", loaded: 4096,
			problems: []string{"loaded qwen3.8:27b with 4096 tokens of context but context_window is 262144: longer prompts are cut without an error. Start Ollama with OLLAMA_CONTEXT_LENGTH=262144"},
		},
		"not loaded": {
			srv:     &checkServer{ollama: true, models: models, caps: chat, trained: 262144},
			gateway: true, embed: "nomic-embed-text", ok: true,
			warnings: []string{"is not loaded"},
		},
		"Modelfile num_ctx too small": {
			srv:     &checkServer{ollama: true, models: models, caps: chat, trained: 262144, params: "num_ctx                        8192\nstop \"<|im_end|>\""},
			gateway: true, embed: "nomic-embed-text",
			problems: []string{"sets num_ctx 8192"},
		},
		"context_window beyond the model": {
			srv:     &checkServer{ollama: true, models: models, caps: chat, trained: 131072, loaded: 131072},
			gateway: true, embed: "nomic-embed-text", loaded: 131072,
			problems: []string{"takes at most 131072", "loaded qwen3.8:27b with 131072"},
		},
		"model not pulled": {
			srv:     &checkServer{ollama: true, models: []string{"nomic-embed-text:latest"}, caps: chat},
			gateway: true, embed: "nomic-embed-text",
			problems: []string{"does not have qwen3.8:27b"},
		},
		"embedding model missing": {
			srv:     &checkServer{ollama: true, models: []string{"qwen3.8:27b"}, caps: chat, trained: 262144, loaded: 262144},
			gateway: true, embed: "nomic-embed-text", loaded: 262144,
			problems: []string{"embedding model nomic-embed-text"},
		},
		"embedding model cannot embed": {
			srv:     &checkServer{ollama: true, models: []string{"qwen3.8:27b"}, caps: chat, trained: 262144, loaded: 262144},
			gateway: true, embed: "qwen3.8:27b", loaded: 262144,
			problems: []string{"qwen3.8:27b is not an embedding model"},
		},
		"no embedding model while embeddings are routed here": {
			srv:     &checkServer{ollama: true, models: models, caps: chat, trained: 262144, loaded: 262144},
			gateway: true, loaded: 262144,
			problems: []string{"no embedding_model is set"},
		},
		"other OpenAI-compatible server": {
			srv:     &checkServer{models: models},
			gateway: true, embed: "nomic-embed-text", ok: true,
			warnings: []string{"does not say how much context"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(tc.srv)
			defer srv.Close()
			got := checkRegistry(srv.URL, tc.gateway, tc.embed).CheckLocal(context.Background())
			require.Equal(t, len(got.Providers), 1, "only local providers are checked")
			c := got.Providers[0]
			assert.True(t, c.Reachable)
			assert.Equal(t, c.OK, tc.ok, strings.Join(c.Problems, "; "))
			assert.Equal(t, got.OK, tc.ok)
			assert.Equal(t, c.LoadedContext, tc.loaded)
			assert.Equal(t, c.Gateway, true)
			assert.Equal(t, got.Gateway, "ollama")
			assert.True(t, !strings.Contains(strings.Join(c.Tasks, ","), "review"), "review goes to the cloud provider")
			assert.Equal(t, len(c.Problems), len(tc.problems), strings.Join(c.Problems, "; "))
			for i, want := range tc.problems {
				if i < len(c.Problems) {
					assert.True(t, strings.Contains(c.Problems[i], want), c.Problems[i]+" should say "+want)
				}
			}
			for _, want := range tc.warnings {
				assert.True(t, strings.Contains(strings.Join(c.Warnings, "; "), want), "warning: "+want)
			}
			if !tc.ok && tc.gateway {
				assert.True(t, len(got.Problems) == 1 && strings.Contains(got.Problems[0], "gateway"), "a failing gateway provider is named")
			}
			tc.srv.mu.Lock()
			defer tc.srv.mu.Unlock()
			for _, b := range tc.srv.bodies {
				assert.True(t, !strings.Contains(b, "messages") && !strings.Contains(b, "prompt") && !strings.Contains(b, "input"), "only model names are sent: "+b)
			}
		})
	}
}

func TestCheckLocalGatewayAndReach(t *testing.T) {
	srv := httptest.NewServer(&checkServer{ollama: true, models: []string{"qwen3.8:27b"}, trained: 262144, loaded: 262144})
	got := checkRegistry(srv.URL, false, "").CheckLocal(context.Background())
	assert.Equal(t, got.Gateway, "")
	assert.True(t, strings.Contains(strings.Join(got.Warnings, ";"), "no local provider has gateway: true"))

	srv.Close()
	got = checkRegistry(srv.URL, true, "").CheckLocal(context.Background())
	c := got.Providers[0]
	assert.False(t, c.Reachable)
	assert.False(t, got.OK)
	require.Equal(t, len(c.Problems), 1)
	assert.True(t, strings.Contains(c.Problems[0], "does not answer"), c.Problems[0])
	assert.True(t, !strings.Contains(c.Problems[0], srv.URL), "the URL is not repeated: "+c.Problems[0])

	_, ok := checkRegistry(srv.URL, true, "").Check(context.Background(), "cloud")
	assert.False(t, ok, "cloud providers are not probed")
}
