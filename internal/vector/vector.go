// Package vector is the nearest-neighbour index for agent-memory entity
// embeddings. Flat is exact and in-process (tests, small installs); package
// milvus serves production.
//
// Tenant isolation is part of the contract, not a caller convention: every
// item carries the organisation it belongs to, every search names one, and
// implementations drop any hit whose stored tenant differs, even if the
// backend's own filter was wrong.
package vector

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/parisnakitakejser/codesynapse/internal/memory"
)

// Item is one entity's vector.
type Item struct {
	ID string
	// Tenant is the organisation (memory.TenantOf of the namespace).
	Tenant    string
	Namespace string
	Vector    []float32
}

// Hit is a search result; Score is memory.Similarity (0..1, higher is closer).
type Hit struct {
	ID        string
	Tenant    string
	Namespace string
	Score     float64
}

// Index stores and searches vectors per embedding model.
type Index interface {
	// Upsert stores vectors made by model, replacing earlier ones with the
	// same ID. All have the same length (1 to memory.MaxDimensions).
	Upsert(ctx context.Context, model string, items []Item) error
	// Search returns up to k items of tenant's namespace embedded by model,
	// nearest to vec first.
	Search(ctx context.Context, model, tenant, namespace string, vec []float32, k int) ([]Hit, error)
	// Delete removes items by ID under every model.
	Delete(ctx context.Context, ids []string) error
	Close(ctx context.Context) error
}

// A search returns MinK to MaxK hits (DefaultK when unset): recall should
// add a few strong entities, not a long tail that floods the context. Fewer
// than three gives the graph expansion too little to work from.
const (
	MinK     = 3
	MaxK     = 10
	DefaultK = 8
)

// ErrNoTenant rejects searches and writes without a tenant.
var ErrNoTenant = errors.New("vector: tenant is required")

// Check validates items for Upsert.
func Check(items []Item) error {
	vecs := make(map[string][]float32, len(items))
	for _, it := range items {
		if it.Tenant == "" || it.Namespace == "" || it.ID == "" {
			return fmt.Errorf("%w (item %q)", ErrNoTenant, it.ID)
		}
		vecs[it.ID] = it.Vector
	}
	return memory.CheckVectors(vecs)
}

// Isolate keeps the hits of tenant's namespace, best first, at most k. Every
// Index runs its results through it.
func Isolate(hits []Hit, tenant, namespace string, k int) []Hit {
	out := slices.DeleteFunc(hits, func(h Hit) bool { return h.Tenant != tenant || h.Namespace != namespace })
	slices.SortFunc(out, func(a, b Hit) int { return cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(a.ID, b.ID)) })
	return out[:min(len(out), k)]
}

// ClampK bounds k to [MinK, MaxK], defaulting to DefaultK. Every store's
// nearest-neighbour search uses it, so they answer alike.
func ClampK(k int) int {
	if k <= 0 {
		return DefaultK
	}
	return max(MinK, min(k, MaxK))
}

// Flat is an exact in-process Index.
type Flat struct {
	mu    sync.RWMutex
	items map[string]flatItem
}

type flatItem struct {
	Item
	model string
}

var _ Index = (*Flat)(nil)

// NewFlat returns an empty index.
func NewFlat() *Flat { return &Flat{items: map[string]flatItem{}} }

// Upsert implements Index.
func (f *Flat) Upsert(_ context.Context, model string, items []Item) error {
	if err := Check(items); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, it := range items {
		it.Vector = slices.Clone(it.Vector)
		f.items[it.ID] = flatItem{Item: it, model: model}
	}
	return nil
}

// Search implements Index.
func (f *Flat) Search(_ context.Context, model, tenant, namespace string, vec []float32, k int) ([]Hit, error) {
	if tenant == "" {
		return nil, ErrNoTenant
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	var hits []Hit
	for _, it := range f.items {
		if it.model == model && it.Tenant == tenant && it.Namespace == namespace && len(it.Vector) == len(vec) {
			hits = append(hits, Hit{ID: it.ID, Tenant: it.Tenant, Namespace: it.Namespace, Score: memory.Similarity(vec, it.Vector)})
		}
	}
	return Isolate(hits, tenant, namespace, ClampK(k)), nil
}

// Delete implements Index.
func (f *Flat) Delete(_ context.Context, ids []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		delete(f.items, id)
	}
	return nil
}

// Close implements Index.
func (f *Flat) Close(context.Context) error { return nil }
