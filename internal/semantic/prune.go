package semantic

import (
	"context"
	"errors"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/vector"
)

// Pruning keeps the index to what the published graphs describe. Sync adds a
// vector whenever a symbol's description changes and never removes one, so
// after an ingest Prune clears the vectors of symbols, and of descriptions,
// that the repository's graph no longer has; Forget clears a deleted
// repository; Sweep finds graphs whose repository is gone. All of them work
// in pages of scanPage IDs and stop after maxPages, so one call is bounded
// however large the index grows, and every delete names the code-graph
// tenant and one graph.

const (
	scanPage = 500
	maxPages = 200
)

// ErrNoSweeper says the index cannot page through or remove its items.
var ErrNoSweeper = errors.New("semantic: the vector index cannot remove items")

// PruneStats says what a Prune did.
type PruneStats struct {
	Scanned int // vectors of the graph looked at
	Removed int // stale vectors removed
	// Complete is false when the page limit stopped the scan; the next
	// ingest carries on from the start.
	Complete bool
}

// Live is the set of item IDs the current descriptions of nodes have: the
// vectors a graph keeps.
func Live(nodes []model.Node) map[string]bool {
	out := map[string]bool{}
	for _, n := range nodes {
		if !Embeddable(n) {
			continue
		}
		if id, ok := itemID(n.ID, Text(n)); ok {
			out[id] = true
		}
	}
	return out
}

// Stale returns the IDs of items that live does not hold, in their order.
func Stale(items []vector.Item, live map[string]bool) []string {
	var out []string
	for _, it := range items {
		if !live[it.ID] {
			out = append(out, it.ID)
		}
	}
	return out
}

func (s Store) sweeper() (vector.Sweeper, error) {
	sw, ok := s.Idx.(vector.Sweeper)
	if !ok || s.Idx == nil {
		return nil, ErrNoSweeper
	}
	return sw, nil
}

// Prune removes graph's vectors that are not the current description of a
// symbol in nodes, under every model. Call it after a Sync that succeeded,
// with the nodes of the graph that was published.
func (s Store) Prune(ctx context.Context, graph string, nodes []model.Node) (PruneStats, error) {
	var st PruneStats
	sw, err := s.sweeper()
	if err != nil {
		return st, err
	}
	if graph == "" {
		return st, vector.ErrNoNamespace
	}
	live := Live(nodes)
	after := ""
	for range maxPages {
		page, err := sw.Scan(ctx, Tenant, graph, after, scanPage)
		if err != nil {
			return st, err
		}
		st.Scanned += len(page)
		if stale := Stale(page, live); len(stale) > 0 {
			if err := sw.DeleteIn(ctx, Tenant, graph, stale); err != nil {
				return st, err
			}
			st.Removed += len(stale)
		}
		if len(page) < scanPage {
			st.Complete = true
			return st, nil
		}
		after = page[len(page)-1].ID
	}
	return st, nil
}

// Forget removes every vector of graph, under every model.
func (s Store) Forget(ctx context.Context, graph string) error {
	sw, err := s.sweeper()
	if err != nil {
		return err
	}
	return sw.DeleteNamespace(ctx, Tenant, graph)
}

// SweepStats says what a Sweep did.
type SweepStats struct {
	Scanned int // vectors looked at
	Graphs  int // graphs without a repository whose vectors were removed
	// Next is where the following Sweep starts; "" after a full pass.
	Next string
}

// Sweep looks at up to pages pages of vectors from after onwards and
// removes the vectors of every graph exists says is gone. exists is asked at
// most once per graph and call, right before the delete, so a repository
// added while the sweep runs keeps its vectors. An error from exists stops
// the sweep without deleting.
func (s Store) Sweep(ctx context.Context, after string, pages int, exists func(context.Context, string) (bool, error)) (SweepStats, error) {
	st := SweepStats{Next: after}
	sw, err := s.sweeper()
	if err != nil {
		return st, err
	}
	known := map[string]bool{}
	for range max(1, min(pages, maxPages)) {
		page, err := sw.Scan(ctx, Tenant, "", st.Next, scanPage)
		if err != nil {
			return st, err
		}
		st.Scanned += len(page)
		for _, it := range page {
			if it.Namespace == "" {
				continue
			}
			if _, seen := known[it.Namespace]; seen {
				continue
			}
			ok, err := exists(ctx, it.Namespace)
			if err != nil {
				return st, err
			}
			known[it.Namespace] = ok
			if ok {
				continue
			}
			if err := sw.DeleteNamespace(ctx, Tenant, it.Namespace); err != nil {
				return st, err
			}
			st.Graphs++
		}
		if len(page) < scanPage {
			st.Next = ""
			return st, nil
		}
		st.Next = page[len(page)-1].ID
	}
	return st, nil
}
