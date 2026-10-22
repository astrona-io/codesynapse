package recall

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

	"github.com/parisnakitakejser/codesynapse/internal/memory"
	memstore "github.com/parisnakitakejser/codesynapse/internal/store/memory"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
	"github.com/parisnakitakejser/codesynapse/services/api/internal/llm"
)

// fakeEmbeddings maps texts about money to one direction and the rest to
// another, so "money flow" is near "payments" without sharing a word.
func fakeEmbeddings(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Input []string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var data []item
		for i, in := range req.Input {
			v := []float32{0, 0, 1}
			l := strings.ToLower(in)
			switch {
			case strings.Contains(l, "refund"):
				v = []float32{1, 0.1, 0.6}
			case strings.Contains(l, "payment"), strings.Contains(l, "money"):
				v = []float32{1, 0.1, 0}
			case strings.Contains(l, "billing"):
				v = []float32{0.7, 0.7, 0}
			}
			data = append(data, item{i, v})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func setup(t *testing.T, withEmbeddings bool) (*Service, *memstore.Store) {
	t.Helper()
	st := memstore.New()
	b, err := memory.Prepare("r", []memory.Triple{
		{Subject: "Payments service", SubjectType: "Service", Predicate: "OWNED_BY", Object: "Team Blue", ObjectType: "Team"},
		{Subject: "Team Blue", Predicate: "PREFERS", Object: "Small PRs"},
		{Subject: "Frontend", Predicate: "USES", Object: "Vue"},
	}, "agent", time.Now())
	require.NoError(t, err)
	require.NoError(t, st.UpsertMemory(context.Background(), b))
	var reg *llm.Registry
	if withEmbeddings {
		srv := fakeEmbeddings(t)
		reg = llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{{Name: "ollama", Kind: llm.KindOpenAI, BaseURL: srv.URL + "/v1",
			Model: "qwen", EmbeddingModel: "nomic", TimeoutSeconds: 5}}, Default: "ollama"})
	}
	return New(st, reg, slog.New(slog.NewTextHandler(io.Discard, nil))), st
}

func TestHybridRecall(t *testing.T) {
	ctx := context.Background()
	s, _ := setup(t, true)
	n, err := s.EmbedPending(ctx)
	require.NoError(t, err)
	assert.Equal(t, n, 5, "every entity embedded")
	n, err = s.EmbedPending(ctx)
	require.NoError(t, err)
	assert.Equal(t, n, 0, "nothing left for this model")

	res, err := s.Recall(ctx, "r", Query{Text: "where does the money flow", Depth: 2})
	require.NoError(t, err)
	assert.Equal(t, res.Mode, "hybrid")
	require.True(t, len(res.Entries) >= 1)
	assert.Equal(t, res.Entries[0].Name, "Payments service", "found by meaning, no shared word")
	assert.Equal(t, res.Entries[0].Via, "vector")
	preds := map[string]bool{}
	for _, f := range res.Facts {
		preds[f.Predicate] = true
	}
	assert.True(t, preds["OWNED_BY"] && preds["PREFERS"], "two hops reach Team Blue's preference")
	assert.False(t, preds["USES"], "unrelated facts stay out")

	res, err = s.Recall(ctx, "r", Query{Text: "payments"})
	require.NoError(t, err)
	assert.Equal(t, res.Entries[0].Via, "both")

	res, err = s.Recall(ctx, "r", Query{Text: "thanks!"})
	require.NoError(t, err)
	assert.Equal(t, res.Skipped, "small talk")
	assert.Equal(t, len(res.Facts), 0)
}

func TestRecallWithoutEmbeddings(t *testing.T) {
	s, _ := setup(t, false)
	res, err := s.Recall(context.Background(), "r", Query{Text: "where does the money flow"})
	require.NoError(t, err)
	assert.Equal(t, res.Mode, "names")
	assert.Equal(t, len(res.Entries), 0, "no shared words, no vectors: nothing")
	res, err = s.Recall(context.Background(), "r", Query{Text: "frontend"})
	require.NoError(t, err)
	require.Len(t, res.Facts, 1)
	assert.Equal(t, res.Facts[0].Predicate, "USES")
	_, err = s.EmbedPending(context.Background())
	require.Error(t, err)
}

func TestRecallFusesWithinTheTenant(t *testing.T) {
	ctx := context.Background()
	s, st := setup(t, true)
	mine, theirs := memory.Namespace("org-a", "r"), memory.Namespace("org-b", "r")
	var triples []memory.Triple
	for _, n := range []string{"Payments ledger", "Payments gateway", "Payments refunds", "Payments fraud", "Payments reports",
		"Payments export", "Payments import", "Payments audit", "Payments alerts", "Payments billing"} {
		triples = append(triples, memory.Triple{Subject: n, Predicate: "PART_OF", Object: "Payments"})
	}
	for ns, ts := range map[string][]memory.Triple{mine: triples, theirs: {{Subject: "Payments secret project", Predicate: "OWNED_BY", Object: "Org B"}}} {
		b, err := memory.Prepare(ns, ts, "agent", time.Now())
		require.NoError(t, err)
		require.NoError(t, st.UpsertMemory(ctx, b))
	}
	_, err := s.EmbedPending(ctx)
	require.NoError(t, err)

	res, err := s.Recall(ctx, mine, Query{Text: "payments refunds"})
	require.NoError(t, err)
	assert.Equal(t, res.Mode, "hybrid")
	assert.True(t, len(res.Entries) <= entryLimit, "top_k: at most %d entries, got %d", entryLimit, len(res.Entries))
	// The fake embeds every payments name alike, so only the fusion is
	// checked here; memory.TestRankBM25 and TestFuseRRF check the order.
	seen := map[string]bool{}
	for _, e := range res.Entries {
		if e.Name == "Payments refunds" {
			assert.Equal(t, e.Via, "both", "found by words and by meaning")
		}
		assert.False(t, seen[e.ID], "deduplicated: "+e.Name)
		seen[e.ID] = true
		assert.Equal(t, e.Repo, mine, "both legs stay in the namespace: "+e.Name)
	}
	assert.True(t, seen[memory.EntityID(mine, "payments refunds")], "the entity both legs found is an entry")
	for _, e := range res.Entities {
		assert.False(t, strings.Contains(e.Name, "secret"), "another organisation's memory never appears")
	}
}
