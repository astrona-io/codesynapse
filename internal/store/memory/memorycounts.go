package memory

import (
	"context"
	"slices"

	"github.com/parisnakitakejser/codesynapse/internal/store"
)

// MemoryFactCounts implements store.MemoryStore.
func (s *Store) MemoryFactCounts(ctx context.Context, afterNamespace string, limit int) ([]store.MemoryFactCount, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.mem))
	for ns, m := range s.mem {
		if ns > afterNamespace && len(m.entities) != 0 {
			names = append(names, ns)
		}
	}
	slices.Sort(names)
	names = names[:min(len(names), store.ClampLimit(limit, 100))]
	out := make([]store.MemoryFactCount, 0, len(names))
	for _, ns := range names {
		row := store.MemoryFactCount{Namespace: ns}
		for _, f := range s.mem[ns].facts {
			if f.Current() {
				row.Facts++
			}
		}
		out = append(out, row)
	}
	return out, nil
}
