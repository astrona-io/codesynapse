package sessionmemory

import (
	"context"
	"math"
	"time"

	"github.com/astrona-io/codesynapse/internal/memory"
)

// VectorBackend supplies scoped vector storage for validated graph entities.
type VectorBackend interface {
	WriteSessionVectors(context.Context, Scope, string, map[string][]float32) error
	SearchSessionVectors(context.Context, Scope, string, []float32, int) ([]memory.Scored, error)
}

// SetSessionMemoryEmbeddings accepts only existing entities in the selected session.
func (e Engine) SetSessionMemoryEmbeddings(ctx context.Context, scope Scope, model string, vecs map[string][]float32) error {
	if err := scope.Check(ctx); err != nil {
		return err
	}
	if model == "" || len(model) > 256 || len(vecs) > MaxEntities {
		return ErrLimit
	}
	if err := memory.CheckVectors(vecs); err != nil {
		return err
	}
	d, err := e.Backend.LoadSessionDocument(ctx, scope)
	if err != nil {
		return err
	}
	if err = CheckDocument(d, scope, time.Now()); err != nil {
		return err
	}
	want := map[string]bool{}
	for _, ent := range d.Graph.Entities {
		want[ent.ID] = true
	}
	for id := range vecs {
		if !want[id] {
			return ErrScope
		}
	}
	return e.Backend.(VectorBackend).WriteSessionVectors(ctx, scope, model, vecs)
}

// SessionMemoryNearest revalidates vector hits against the current graph snapshot.
func (e Engine) SessionMemoryNearest(ctx context.Context, scope Scope, model string, vec []float32, k int) ([]memory.Scored, error) {
	if err := scope.Check(ctx); err != nil {
		return nil, err
	}
	if err := memory.CheckVectors(map[string][]float32{"query": vec}); err != nil {
		return nil, err
	}
	before, err := e.Backend.LoadSessionDocument(ctx, scope)
	if err != nil {
		return nil, err
	}
	if err = CheckDocument(before, scope, time.Now()); err != nil {
		return nil, err
	}
	if before.Revision == 0 {
		return []memory.Scored{}, nil
	}
	hits, err := e.Backend.(VectorBackend).SearchSessionVectors(ctx, scope, model, vec, min(max(k, 1), 10))
	if err != nil {
		return nil, err
	}
	d, err := e.Backend.LoadSessionDocument(ctx, scope)
	if err != nil {
		return nil, err
	}
	if err = CheckDocument(d, scope, time.Now()); err != nil {
		return nil, err
	}
	known := map[string]memory.Entity{}
	for _, ent := range d.Graph.Entities {
		known[ent.ID] = ent
	}
	out := []memory.Scored{}
	seen := map[string]bool{}
	for _, hit := range hits[:min(len(hits), 10)] {
		ent, ok := known[hit.ID]
		if ok && !seen[hit.ID] && hit.Repo == scope.Key() && !math.IsNaN(hit.Score) && !math.IsInf(hit.Score, 0) && hit.Score >= 0 && hit.Score <= 1 {
			out = append(out, memory.Scored{Entity: ent, Score: hit.Score})
			seen[hit.ID] = true
		}
	}
	return out, nil
}
