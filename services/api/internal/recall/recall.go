// Package recall finds the memory relevant to a query: an intent gate, then
// entry entities from two legs, vector similarity (local embeddings) and
// BM25 over names, aliases and types, fused by reciprocal rank, then a 1-2
// hop expansion (docs/adr/0001-search.md). Both legs read only the query's
// namespace. It also keeps entity embeddings up to date.
package recall

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/logx"
	"github.com/parisnakitakejser/codesynapse/internal/memory"
	"github.com/parisnakitakejser/codesynapse/internal/store"
	"github.com/parisnakitakejser/codesynapse/internal/telemetry"
	"github.com/parisnakitakejser/codesynapse/services/api/internal/llm"
)

// Tuning.
const (
	// MinScore keeps vector hits at cosine >= 0.4: (1+0.4)/2.
	MinScore    = 0.7
	entryLimit  = 8
	embedBatch  = 64
	embedPeriod = 30 * time.Second
)

// Service answers recall queries and embeds entities in the background.
type Service struct {
	St     store.MemoryStore
	LLM    *llm.Registry // nil: name search only
	Logger *slog.Logger
	kick   chan struct{}
}

// New returns a recall service; call Run for background embedding.
func New(st store.MemoryStore, reg *llm.Registry, logger *slog.Logger) *Service {
	return &Service{St: st, LLM: reg, Logger: logger, kick: make(chan struct{}, 1)}
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
	Mode    string  `json:"mode"`
	Entries []Entry `json:"entries"`
	memory.Graph
}

// Recall runs the gate, finds entry entities and expands them.
func (s *Service) Recall(ctx context.Context, repo string, q Query) (Result, error) {
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
	lex, err := s.St.MemoryLexical(ctx, repo, q.Text, entryLimit)
	if err != nil {
		return res, err
	}
	legs["name"] = lex
	for _, f := range memory.FuseRRF(entryLimit, legs) {
		via := f.Legs[0]
		if len(f.Legs) > 1 {
			via = "both"
		}
		res.Entries = append(res.Entries, Entry{Entity: f.Entity, Score: f.Score, Via: via})
	}
	if len(res.Entries) == 0 {
		return res, nil
	}
	ids := make([]string, len(res.Entries))
	for i, e := range res.Entries {
		ids[i] = e.ID
	}
	sub, err := s.St.MemorySubgraph(ctx, repo, ids, cmp.Or(q.Depth, 1), cmp.Or(q.Limit, 40), q.History)
	if err != nil {
		return res, err
	}
	res.Graph = sub
	return res, nil
}

func (s *Service) nearest(ctx context.Context, repo, text string) ([]memory.Scored, error) {
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
	hits, err := s.St.MemoryNearest(ctx, repo, cfg.EmbeddingModel, vecs[0], entryLimit)
	if err != nil {
		return nil, err
	}
	kept := hits[:0]
	for _, h := range hits {
		if h.Score >= MinScore {
			kept = append(kept, h)
		}
	}
	return kept, nil
}

// Kick asks the embedder to run soon (after new facts were saved).
func (s *Service) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Run embeds entities without a current embedding until ctx ends.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(embedPeriod)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kick:
		}
		if n, err := s.EmbedPending(ctx); err != nil && !errors.Is(err, llm.ErrNoEmbeddings) {
			s.Logger.Warn("embedding memory entities failed", "embedded", n, logx.Error(err))
		}
	}
}

// EmbedPending embeds up to a few batches of entities and returns how many.
func (s *Service) EmbedPending(ctx context.Context) (int, error) {
	if s.LLM == nil {
		return 0, llm.ErrNoEmbeddings
	}
	name, cfg, err := s.LLM.EmbeddingProvider()
	if err != nil {
		return 0, err
	}
	done := 0
	for range 10 { // bounded work per round
		todo, err := s.St.MemoryEmbeddingTodo(ctx, cfg.EmbeddingModel, embedBatch)
		if err != nil || len(todo) == 0 {
			return done, err
		}
		// A background batch can span organisations; split it before model and
		// database calls so one tenant never receives another tenant's usage.
		groups := map[string][]memory.Entity{}
		var tenants []string
		for _, entity := range todo {
			tenant := memory.OrgOf(entity.Repo)
			if _, ok := groups[tenant]; !ok {
				tenants = append(tenants, tenant)
			}
			groups[tenant] = append(groups[tenant], entity)
		}
		slices.Sort(tenants)
		for _, tenant := range tenants {
			groupCtx := telemetry.WithTenant(ctx, tenant)
			groupCtx = telemetry.WithModel(groupCtx, name, cfg.EmbeddingModel)
			if err := s.embedEntities(groupCtx, name, cfg.EmbeddingModel, groups[tenant]); err != nil {
				return done, err
			}
			done += len(groups[tenant])
		}
	}
	return done, nil
}

func (s *Service) embedEntities(ctx context.Context, provider, model string, entities []memory.Entity) error {
	texts := make([]string, len(entities))
	for i, entity := range entities {
		texts[i] = memory.EmbeddingText(entity)
	}
	vecs, err := s.LLM.Embed(ctx, provider, texts)
	if err != nil {
		return err
	}
	set := make(map[string][]float32, len(entities))
	for i, entity := range entities {
		set[entity.ID] = vecs[i]
	}
	if err := s.St.SetMemoryEmbeddings(ctx, model, set); err != nil {
		return err
	}
	telemetry.EmbeddedEntities.AddContext(ctx, float64(len(set)), model)
	return nil
}
