package semantic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// fake maps a text to a vector by the words it holds.
type fake struct{}

func (fake) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = []float32{b(strings.Contains(t, "scale")), b(strings.Contains(t, "queue")), 0.01}
	}
	return out, nil
}

func b(ok bool) float32 {
	if ok {
		return 1
	}
	return 0
}

func TestRankOrdersByMeaning(t *testing.T) {
	nodes := []model.Node{
		{ID: "a", Kind: model.KindFunction, QualifiedName: "pkg.Scale", Summary: "scale workers"},
		{ID: "b", Kind: model.KindFunction, QualifiedName: "pkg.Enqueue", Summary: "queue a job"},
		{ID: "c", Kind: model.KindFunction, QualifiedName: "pkg.Other", Summary: "nothing"},
	}
	ix, err := Build(context.Background(), fake{}, nodes)
	require.NoError(t, err)
	assert.Equal(t, ix.Len(), 3)
	hits, err := ix.Rank(context.Background(), "scale the workers", 2)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, hits[0].ID, "a")
	assert.Greater(t, hits[0].Score, hits[1].Score)
}

func TestTextRedacts(t *testing.T) {
	n := model.Node{Kind: model.KindFunction, QualifiedName: "p.F", Signature: "func F()", Summary: "Does it with AKIAIOSFODNN7EXAMPLE token."}
	txt := Text(n)
	assert.Contains(t, txt, "p.F")
	assert.NotContains(t, txt, "AKIAIOSFODNN7EXAMPLE")
	assert.True(t, len(txt) < 500)
}

func TestOllamaIsLocalOnly(t *testing.T) {
	for _, u := range []string{"https://api.example.com", "http://8.8.8.8:11434", "http://ollama.example.com"} {
		_, err := NewOllama(u, "m")
		assert.Error(t, err, u)
	}
	for _, u := range []string{"http://127.0.0.1:11434", "http://localhost:11434", "http://10.0.0.5:11434"} {
		_, err := NewOllama(u, "m")
		assert.NoError(t, err, u)
	}
}

func TestOllamaEmbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Model string
			Input []string
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, in.Model, "m")
		out := make([][]float32, len(in.Input))
		for i := range out {
			out[i] = []float32{1, 0}
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"embeddings": out}))
	}))
	t.Cleanup(srv.Close)
	o, err := NewOllama(srv.URL, "m")
	require.NoError(t, err)
	vecs, err := o.Embed(context.Background(), []string{"a", "b"})
	require.NoError(t, err)
	assert.Len(t, vecs, 2)
}
