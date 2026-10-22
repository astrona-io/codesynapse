package hugegraphstore

import (
	"context"

	"github.com/parisnakitakejser/codesynapse/internal/memory"
	"github.com/parisnakitakejser/codesynapse/internal/store"
)

// This operator-only enumeration returns namespace IDs, never memory content.
// Each aggregate then starts from that namespace's tenant and namespace filters.
const memoryFactCounts = `def spaces = g.V().hasLabel('mem_entity').values('ns').is(gt(after)).dedup().order().limit(limit).toList()
spaces.collect { ns ->
  def split = ns.indexOf('|')
  def tenant = split < 0 ? noTenant : ns.substring(0, split)
  def n = g.E().hasLabel('fact').has('tenant_id', tenant).has('ns', ns).filter { it.get().property('valid_to').orElse('') == '' }.values('fid').dedup().count().next()
  [namespace: ns, facts: n]
}`

// MemoryFactCounts implements store.MemoryStore.
func (s *Store) MemoryFactCounts(ctx context.Context, afterNamespace string, limit int) ([]store.MemoryFactCount, error) {
	out := []store.MemoryFactCount{}
	err := s.read(ctx, memoryFactCounts, map[string]any{"after": afterNamespace, "noTenant": memory.NoTenant, "limit": store.ClampLimit(limit, 100)}, &out)
	return out, err
}
