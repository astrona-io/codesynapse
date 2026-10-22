// Package recall finds the memory relevant to a query: an intent gate, then
// entry entities from two legs, vector similarity (local embeddings) and
// BM25 over names, aliases and types, fused by reciprocal rank, then a 1-2
// hop expansion (docs/adr/0001-search.md). Both legs read only the query's
// namespace. It also keeps entity embeddings up to date.
package recall

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/logx"
	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/vector"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

// Tuning.
const (
	// MinScore keeps vector hits at cosine >= 0.4: (1+0.4)/2.
	MinScore          = 0.7
	entryLimit        = 8
	lexicalCandidates = vector.MaxK
	maxQueryBytes     = 4096
	maxResponseBytes  = 32 << 10
	maxFacts          = 80
	recallTimeout     = 10 * time.Second
	vectorTimeout     = 2 * time.Second
	embedBatch        = 64
)

// Service answers recall queries and indexes explicitly authorized namespaces.
type Service struct {
	St     store.MemoryStore
	LLM    *llm.Registry // nil: name search only
	Logger *slog.Logger
}

// New returns a recall service; indexing runs under an authorized caller context.
func New(st store.MemoryStore, reg *llm.Registry, logger *slog.Logger) *Service {
	return &Service{St: st, LLM: reg, Logger: logger}
}

// Query is a recall request.
type Query struct {
	Text    string `json:"query"`
	Depth   int    `json:"depth"`
	Limit   int    `json:"limit"`
	History bool   `json:"history"`
}

// Entry is an entity recall started from and why. Score is the fused
// reciprocal-rank score, comparable only within one result.
type Entry struct {
	memory.Entity
	Score float64 `json:"score"`
	Via   string  `json:"via"` // vector, name or both
}

// Result is what recall found.
type Result struct {
	// Skipped says why no lookup ran (the intent gate), else "".
	Skipped string `json:"skipped,omitempty"`
	// Mode is hybrid (vectors and names) or names (no embedding model).
	Mode      string  `json:"mode"`
	Entries   []Entry `json:"entries"`
	Truncated bool    `json:"truncated,omitempty"`
	memory.Graph
}

// Recall runs the gate, finds entry entities and expands them.
func (s *Service) Recall(ctx context.Context, repo string, q Query) (Result, error) {
	if _, err := execution.Require(ctx); err != nil {
		return Result{}, err
	}
	ctx = telemetry.WithTenant(ctx, memory.OrgOf(repo))
	telemetry.AgentMemoryRecalls.IncContext(ctx, telemetry.Tenant(ctx))
	res, err := s.recall(ctx, repo, q)
	if err == nil {
		telemetry.MemoryRecalls.IncContext(ctx, telemetry.Tenant(ctx), res.Mode) // names or hybrid
	}
	return res, err
}

func (s *Service) recall(ctx context.Context, repo string, q Query) (Result, error) {
	res := Result{Mode: "names", Entries: []Entry{}, Graph: memory.Graph{Notice: memory.Notice, Entities: []memory.Entity{}, Facts: []memory.Fact{}}}
	if len(q.Text) > maxQueryBytes {
		return res, fmt.Errorf("recall query exceeds %d bytes", maxQueryBytes)
	}
	ctx, cancel := context.WithTimeout(ctx, recallTimeout)
	defer cancel()
	if why := memory.RecallGate(q.Text); why != "" {
		res.Skipped = why
		return res, nil
	}
	legs := map[string][]memory.Scored{}
	if hits, err := s.nearest(ctx, repo, q.Text); err == nil {
		res.Mode = "hybrid"
		legs["vector"] = hits
	} else if !errors.Is(err, llm.ErrNoEmbeddings) {
		// An embedding failure degrades to names; recall must keep working.
		logx.FromContext(ctx).Warn("vector recall failed; using names", logx.Error(err))
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	lex, err := s.St.MemoryLexical(ctx, repo, q.Text, lexicalCandidates)
	if err != nil {
		return res, err
	}
	legs["name"] = candidates(repo, lex, lexicalCandidates)
	res.Truncated = len(lex) >= lexicalCandidates || len(legs["vector"]) >= vector.MaxK
	fused := memory.FuseRRF(lexicalCandidates+vector.MaxK, legs)
	if len(fused) == 0 {
		return res, nil
	}
	ids := make([]string, len(fused))
	for i, e := range fused {
		ids[i] = e.ID
	}
	depth := min(max(q.Depth, 1), 2)
	limit := min(store.ClampLimit(q.Limit, 40), maxFacts)
	sub, err := s.St.MemorySubgraph(ctx, repo, ids, depth, limit, q.History)
	if err != nil {
		return res, err
	}
	return hydrate(res, sub, legs, repo, q.Text, depth, limit, q.History), nil
}

func (s *Service) nearest(ctx context.Context, repo, text string) ([]memory.Scored, error) {
	ctx, cancel := context.WithTimeout(ctx, vectorTimeout)
	defer cancel()
	if s.LLM == nil {
		return nil, llm.ErrNoEmbeddings
	}
	name, cfg, err := s.LLM.EmbeddingProvider()
	if err != nil {
		return nil, err
	}
	ctx = telemetry.WithModel(ctx, name, cfg.EmbeddingModel)
	vecs, err := s.LLM.Embed(ctx, name, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, errors.New("embedding response has the wrong vector count")
	}
	if err := memory.CheckVectors(map[string][]float32{"query": vecs[0]}); err != nil {
		return nil, err
	}
	hits, err := s.St.MemoryNearest(ctx, repo, cfg.EmbeddingModel, vecs[0], vector.MaxK)
	if err != nil {
		return nil, err
	}
	kept := []memory.Scored{}
	for _, h := range candidates(repo, hits, vector.MaxK) {
		if h.Score >= MinScore && h.Score <= 1 {
			kept = append(kept, h)
		}
	}
	return kept, nil
}

// IndexRepository embeds only the namespace authorized by the initiating runtime.
func (s *Service) IndexRepository(ctx context.Context, namespace string, authorize func(context.Context) error) (int, error) {
	id, err := execution.Require(ctx)
	if err != nil {
		return 0, err
	}
	if memory.OrgOf(namespace) != id.TenantID || authorize == nil {
		return 0, errors.New("repository indexing requires an authorized tenant namespace")
	}
	if err := authorize(ctx); err != nil {
		return 0, err
	}
	if s.LLM == nil {
		return 0, llm.ErrNoEmbeddings
	}
	name, cfg, err := s.LLM.EmbeddingProvider()
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, recallTimeout)
	defer cancel()
	ctx = telemetry.WithTenant(ctx, id.TenantID)
	graph, err := s.St.SearchMemory(ctx, namespace, "", store.MaxLimit, false)
	if err != nil {
		return 0, err
	}
	entities := []memory.Entity{}
	for _, e := range graph.Entities {
		if validEntity(namespace, e) {
			entities = append(entities, e)
		}
	}
	done := 0
	for start := 0; start < len(entities); start += embedBatch {
		batch := entities[start:min(start+embedBatch, len(entities))]
		if err := s.embedEntities(ctx, name, cfg.EmbeddingModel, batch, authorize); err != nil {
			return done, err
		}
		done += len(batch)
	}
	return done, nil
}

func (s *Service) embedEntities(ctx context.Context, provider, model string, entities []memory.Entity, authorize func(context.Context) error) error {
	texts := make([]string, len(entities))
	for i, entity := range entities {
		texts[i] = memory.EmbeddingText(entity)
	}
	vecs, err := s.LLM.Embed(ctx, provider, texts)
	if err != nil {
		return err
	}
	if len(vecs) != len(entities) {
		return errors.New("embedding response has the wrong vector count")
	}
	set := make(map[string][]float32, len(entities))
	for i, entity := range entities {
		set[entity.ID] = vecs[i]
	}
	if err := memory.CheckVectors(set); err != nil {
		return err
	}
	if err := authorize(ctx); err != nil {
		return err
	}
	if err := s.St.SetMemoryEmbeddings(ctx, model, set); err != nil {
		return err
	}
	telemetry.EmbeddedEntities.AddContext(ctx, float64(len(set)), model)
	return nil
}
