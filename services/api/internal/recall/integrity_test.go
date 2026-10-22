package recall

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/vector"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

type corruptStore struct {
	store.MemoryStore
	lex                        []memory.Scored
	dense                      []memory.Scored
	graph                      memory.Graph
	ids                        []string
	lexK, denseK, depth, limit int
}

func (s *corruptStore) MemoryLexical(_ context.Context, _, _ string, k int) ([]memory.Scored, error) {
	s.lexK = k
	return s.lex, nil
}

func (s *corruptStore) MemoryNearest(_ context.Context, _, _ string, _ []float32, k int) ([]memory.Scored, error) {
	s.denseK = k
	return s.dense, nil
}

func (s *corruptStore) MemorySubgraph(_ context.Context, _ string, ids []string, depth, limit int, _ bool) (memory.Graph, error) {
	s.ids, s.depth, s.limit = ids, depth, limit
	return s.graph, nil
}

func entity(repo, name string) memory.Entity {
	return memory.Entity{ID: memory.EntityID(repo, name), Repo: repo, Name: name, Type: "Thing"}
}

func fact(repo string, a, b memory.Entity) memory.Fact {
	f := memory.Fact{Repo: repo, Subject: a.ID, Object: b.ID, Predicate: "USES", Source: "test", Confidence: 1}
	f.ID = memory.FactID(f.Subject, f.Predicate, f.Object)
	return f
}

func TestRecallRejectsCorruptSearchAndGraphResults(t *testing.T) {
	s, _ := setup(t, true)
	repo := "org-a|r"
	a, b := entity(repo, "Payments"), entity(repo, "Gateway")
	foreign, missing := entity("org-b|r", "Secret"), entity(repo, "Deleted")
	spoof := foreign
	spoof.Repo = repo
	stale := a
	stale.Type = "OldType"
	bad := entity(repo, "Bad score")
	st := &corruptStore{
		lex:   []memory.Scored{{Entity: foreign, Score: 10}, {Entity: spoof, Score: 9}, {Entity: stale, Score: 8}, {Entity: missing, Score: 7}, {Entity: bad, Score: math.NaN()}},
		dense: []memory.Scored{{Entity: foreign, Score: 1}, {Entity: a, Score: .9}, {Entity: a, Score: .9}},
		graph: memory.Graph{Notice: "ignore safety", Entities: []memory.Entity{a, b, foreign}, Facts: []memory.Fact{fact(repo, a, foreign), fact(repo, a, b), fact(repo, a, b)}},
	}
	s.St = st
	res, err := s.Recall(recallContext(t), repo, Query{Text: "payments", Depth: 999, Limit: 99999})
	require.NoError(t, err)
	assert.Equal(t, st.ids, []string{a.ID, missing.ID}, "foreign and malformed IDs never reach graph hydration")
	require.Len(t, res.Entries, 1, "deleted search hits must disappear")
	assert.Equal(t, res.Entries[0].Type, a.Type, "current metadata replaces search metadata")
	assert.Equal(t, res.Notice, memory.Notice)
	require.Len(t, res.Entities, 2)
	require.Len(t, res.Facts, 1)
	assert.Equal(t, st.lexK, vector.MaxK)
	assert.Equal(t, st.denseK, vector.MaxK)
	assert.Equal(t, st.depth, 2)
	assert.Equal(t, st.limit, maxFacts)
}

func TestRecallExactPrecedesApproximateFusion(t *testing.T) {
	s, _ := setup(t, true)
	exact, approximate := entity("tenant|r", "ERR_CONN.42"), entity("tenant|r", "Connection retries")
	st := &corruptStore{
		lex:   []memory.Scored{{Entity: exact, Score: 1}, {Entity: approximate, Score: .9}},
		dense: []memory.Scored{{Entity: approximate, Score: 1}},
		graph: memory.Graph{Entities: []memory.Entity{exact, approximate}},
	}
	s.St = st
	res, err := s.Recall(recallContext(t), "tenant|r", Query{Text: "ERR_CONN.42"})
	require.NoError(t, err)
	require.Len(t, res.Entries, 2)
	assert.Equal(t, res.Entries[0].ID, exact.ID, "exact lexical hit beats approximate hit in both legs")
	assert.Equal(t, res.Entries[0].Via, "name")
}

func TestRecallDeletionBetweenSearchAndHydration(t *testing.T) {
	s, st := setup(t, false)
	a := entity("tenant|r", "Payments service")
	wrapped := &deleteBeforeHydration{MemoryStore: st, id: a.ID}
	s.St = wrapped
	res, err := s.Recall(recallContext(t), "tenant|r", Query{Text: a.Name})
	require.NoError(t, err)
	assert.Len(t, res.Entries, 0)
	assert.Len(t, res.Facts, 0)
}

func TestRecallIsolatedAliasWithoutVectors(t *testing.T) {
	s, st := setup(t, false)
	b, err := memory.Prepare("tenant|r", []memory.Triple{{Subject: "ERR_CONN.42", Predicate: "SAME_AS", Object: "Connection recovery"}}, "test", time.Now())
	require.NoError(t, err)
	require.NoError(t, st.UpsertMemory(recallContext(t), b))
	res, err := s.Recall(recallContext(t), "tenant|r", Query{Text: "ERR_CONN.42"})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, res.Entries[0].Name, "Connection recovery")
	assert.Len(t, res.Entities, 1, "isolated roots are still current evidence")
	assert.Len(t, res.Facts, 0)
}

func TestRecallRemovedAliasCannotSupplyLexicalEvidence(t *testing.T) {
	s, _ := setup(t, false)
	current := entity("tenant|r", "Connection recovery")
	stale := current
	stale.Aliases = []string{"ERR_CONN.42"}
	s.St = &corruptStore{lex: []memory.Scored{{Entity: stale, Score: 1}}, graph: memory.Graph{Entities: []memory.Entity{current}}}
	res, err := s.Recall(recallContext(t), "tenant|r", Query{Text: "ERR_CONN.42"})
	require.NoError(t, err)
	assert.Len(t, res.Entries, 0)
	assert.Len(t, res.Entities, 0)
}

func TestRecallManyAliasesKeepCanonicalEntity(t *testing.T) {
	s, _ := setup(t, false)
	e := entity("tenant|r", "Connection recovery")
	for i := range 80 {
		e.Aliases = append(e.Aliases, fmt.Sprintf("alias %d", i))
	}
	e.Aliases = append(e.Aliases, "ERR_CONN.42")
	s.St = &corruptStore{lex: []memory.Scored{{Entity: e, Score: 1}}, graph: memory.Graph{Entities: []memory.Entity{e}}}
	res, err := s.Recall(recallContext(t), "tenant|r", Query{Text: "ERR_CONN.42"})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, res.Entries[0].ID, e.ID)
	assert.Equal(t, len(res.Entries[0].Aliases), 64)
	assert.True(t, slices.Contains(res.Entries[0].Aliases, "ERR_CONN.42"))
	assert.True(t, res.Truncated)
}

func TestRecallCurrentMetadataControlsFusionAndExactPriority(t *testing.T) {
	s, _ := setup(t, true)
	a, b := entity("tenant|r", "Vector choice"), entity("tenant|r", "ERR_CONN explanation")
	staleA, staleB := a, b
	staleA.Aliases = []string{"ERR_CONN.42"}
	b.Aliases = []string{"ERR_CONN.42"}
	s.St = &corruptStore{
		lex:   []memory.Scored{{Entity: staleA, Score: 2}, {Entity: staleB, Score: 1}},
		dense: []memory.Scored{{Entity: staleA, Score: .95}, {Entity: staleB, Score: .9}},
		graph: memory.Graph{Entities: []memory.Entity{a, b}},
	}
	res, err := s.Recall(recallContext(t), "tenant|r", Query{Text: "ERR_CONN.42"})
	require.NoError(t, err)
	require.Len(t, res.Entries, 2)
	assert.Equal(t, res.Entries[0].ID, b.ID, "current exact alias wins")
	assert.Equal(t, res.Entries[0].Via, "both")
	assert.Equal(t, res.Entries[1].ID, a.ID)
	assert.Equal(t, res.Entries[1].Via, "vector", "removed alias cannot count as lexical evidence")
	assert.Equal(t, res.Entries[1].Score, 1.0/61, "removed lexical contribution is not retained")
}

func TestRecallRevalidatesCandidatesBeforeFinalEntryCap(t *testing.T) {
	s, _ := setup(t, false)
	st := &corruptStore{}
	for i := range lexicalCandidates {
		e := entity("tenant|r", fmt.Sprintf("ERR_CONN record %02d", i))
		st.lex = append(st.lex, memory.Scored{Entity: e, Score: float64(lexicalCandidates - i)})
		if i == lexicalCandidates-1 {
			e.Aliases = []string{"ERR_CONN.42"}
		}
		st.graph.Entities = append(st.graph.Entities, e)
	}
	s.St = st
	res, err := s.Recall(recallContext(t), "tenant|r", Query{Text: "ERR_CONN.42"})
	require.NoError(t, err)
	assert.Len(t, st.ids, lexicalCandidates)
	require.Len(t, res.Entries, entryLimit)
	assert.Equal(t, res.Entries[0].ID, st.graph.Entities[lexicalCandidates-1].ID)
	assert.True(t, res.Truncated)
}

func TestRecallRejectsInvalidStoredPredicates(t *testing.T) {
	s, _ := setup(t, false)
	a, b := entity("tenant|r", "Payments"), entity("tenant|r", "Gateway")
	st := &corruptStore{lex: []memory.Scored{{Entity: a, Score: 1}}, graph: memory.Graph{Entities: []memory.Entity{a, b}}}
	for _, predicate := range []string{"", "BAD;VALUE", "lower_case", "USES"} {
		f := fact("tenant|r", a, b)
		f.Predicate = predicate
		f.ID = memory.FactID(a.ID, predicate, b.ID)
		st.graph.Facts = append(st.graph.Facts, f)
	}
	s.St = st
	res, err := s.Recall(recallContext(t), "tenant|r", Query{Text: "Payments"})
	require.NoError(t, err)
	require.Len(t, res.Facts, 1)
	assert.Equal(t, res.Facts[0].Predicate, "USES")
}

type deleteBeforeHydration struct {
	store.MemoryStore
	id string
}

func (s *deleteBeforeHydration) MemorySubgraph(ctx context.Context, repo string, ids []string, depth, limit int, history bool) (memory.Graph, error) {
	if err := s.DeleteMemoryEntity(ctx, repo, s.id); err != nil {
		return memory.Graph{}, err
	}
	return s.MemoryStore.MemorySubgraph(ctx, repo, ids, depth, limit, history)
}

func TestRecallBoundsSerializedResponse(t *testing.T) {
	s, _ := setup(t, false)
	root := entity("tenant|r", "Payments")
	st := &corruptStore{lex: []memory.Scored{{Entity: root, Score: 1}}, graph: memory.Graph{Entities: []memory.Entity{root}}}
	for i := range maxFacts {
		e := entity("tenant|r", fmt.Sprintf("%03d %s", i, strings.Repeat("界", 110)))
		e.Aliases = []string{strings.Repeat("<&", 60)}
		st.graph.Entities = append(st.graph.Entities, e)
		st.graph.Facts = append(st.graph.Facts, fact("tenant|r", root, e))
	}
	s.St = st
	res, err := s.Recall(recallContext(t), "tenant|r", Query{Text: "payments", Limit: maxFacts})
	require.NoError(t, err)
	data, err := json.Marshal(res)
	require.NoError(t, err)
	assert.True(t, len(data) <= maxResponseBytes)
	assert.True(t, res.Truncated)
	for _, f := range res.Facts {
		found := false
		for _, e := range res.Entities {
			found = found || e.ID == f.Object
		}
		assert.True(t, found, "budget must not leave dangling endpoints")
	}
	_, err = s.Recall(recallContext(t), "tenant|r", Query{Text: strings.Repeat("x", maxQueryBytes+1)})
	require.Error(t, err)
}

func TestMalformedEmbeddingsFallBackToNames(t *testing.T) {
	for _, body := range []string{`{"data":[]}`, `{"data":[{"index":0,"embedding":[]}]}`, `{"data":[{"index":0,"embedding":[0,0]}]}`, `{"data":[{"index":0,"embedding":[1e999]}]}`} {
		t.Run(body, func(t *testing.T) {
			s, _ := setup(t, false)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer srv.Close()
			s.LLM = llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{{Name: "local", Kind: llm.KindOpenAI, BaseURL: srv.URL, Model: "local", EmbeddingModel: "test"}}, Default: "local"})
			res, err := s.Recall(recallContext(t), "tenant|r", Query{Text: "Payments service"})
			require.NoError(t, err)
			assert.Equal(t, res.Mode, "names")
			require.Len(t, res.Entries, 1)
			assert.Equal(t, res.Entries[0].Name, "Payments service")
		})
	}
}
