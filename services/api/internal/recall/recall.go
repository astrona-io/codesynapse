// Package recall finds the memory relevant to a query: an intent gate, then
// entry entities by vector similarity (local embeddings) and by name, then
// a 1-2 hop expansion. It also keeps entity embeddings up to date.
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

// Entry is an entity recall started from and why.
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
	res, err := s.recall(ctx, repo, q)
	if err == nil {
		telemetry.MemoryRecalls.Inc(telemetry.TenantLabel(memory.OrgOf(repo)), res.Mode) // names or hybrid
	}
	return res, err
}

func (s *Service) recall(ctx context.Context, repo string, q Query) (Result, error) {
	res := Result{Mode: "names", Entries: []Entry{}, Graph: memory.Graph{Notice: memory.Notice, Entities: []memory.Entity{}, Facts: []memory.Fact{}}}
	if why := memory.RecallGate(q.Text); why != "" {
		res.Skipped = why
		return res, nil
	}
	entries := map[string]*Entry{}
	if hits, err := s.nearest(ctx, repo, q.Text); err == nil {
		res.Mode = "hybrid"
		for _, h := range hits {
			entries[h.ID] = &Entry{Entity: h.Entity, Score: h.Score, Via: "vector"}
		}
	} else if !errors.Is(err, llm.ErrNoEmbeddings) {
		// An embedding failure degrades to names; recall must keep working.
		logx.FromContext(ctx).Warn("vector recall failed; using names", logx.Error(err))
	}
	g, err := s.St.SearchMemory(ctx, repo, q.Text, entryLimit, q.History)
	if err != nil {
		return res, err
	}
	words := memory.Words(q.Text)
	for _, e := range g.Entities {
		sc := memory.Score(e, words)
		if sc == 0 {
			continue // an endpoint of a matched entity, not a match itself
		}
		if cur, ok := entries[e.ID]; ok {
			cur.Via = "both"
			continue
		}
		entries[e.ID] = &Entry{Entity: e, Score: float64(sc) / float64(len(words)), Via: "name"}
	}
	for _, e := range entries {
		res.Entries = append(res.Entries, *e)
	}
	slices.SortFunc(res.Entries, func(a, b Entry) int { return cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(a.ID, b.ID)) })
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
		texts := make([]string, len(todo))
		for i, e := range todo {
			texts[i] = memory.EmbeddingText(e)
		}
		vecs, err := s.LLM.Embed(ctx, name, texts)
		if err != nil {
			return done, err
		}
		set := make(map[string][]float32, len(todo))
		for i, e := range todo {
			set[e.ID] = vecs[i]
		}
		telemetry.EmbeddedEntities.Add(float64(len(set)), cfg.EmbeddingModel)
		if err := s.St.SetMemoryEmbeddings(ctx, cfg.EmbeddingModel, set); err != nil {
			return done, err
		}
		done += len(todo)
	}
	return done, nil
}
