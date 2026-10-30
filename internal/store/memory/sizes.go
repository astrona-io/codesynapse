package memory

import (
	"context"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/telemetry"
)

var _ store.Sizer = (*Store)(nil)

// StoreSizes implements store.Sizer: code graph nodes, and edges per edge
// kind in the order of model.AllEdgeKinds. The in-memory store has no
// replace lease to wait for and reports no bytes; its db_system is not one
// the gauges keep.
func (s *Store) StoreSizes(context.Context) ([]telemetry.StoreSize, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var nodes int64
	edges := map[model.EdgeKind]int64{}
	for _, g := range s.repos {
		nodes += int64(len(g.nodes))
		for _, out := range g.out {
			for _, e := range out {
				edges[e.Kind]++
			}
		}
	}
	out := []telemetry.StoreSize{{System: s.System(), Kind: "node", Rows: nodes, Bytes: -1}}
	for _, k := range model.AllEdgeKinds {
		if n := edges[k]; n > 0 {
			out = append(out, telemetry.StoreSize{System: s.System(), Kind: string(k), Rows: n, Bytes: -1})
		}
	}
	return out, nil
}
