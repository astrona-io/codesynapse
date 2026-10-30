package telemetry

import (
	"context"
	"sync"
)

// Store sizes (docs/PLATFORM_STATS_PLAN.md, phase 6): rows and bytes per
// database and kind, sampled once a day by the api-loops holder. They belong
// to no tenant. db_system is one of the three stores; kind is a NebulaGraph
// tag or edge type, a fixed PostgreSQL table or "database", or a Milvus
// collection family, each registered by the package that owns the store.
// NebulaGraph and Milvus report rows only, so they have no bytes series.
var (
	StoreRows  = Default.Gauge("platform_codesynapse_store_rows", "Rows per database and kind (tag, edge type, table, collection family), sampled daily.", "db_system", "kind")
	StoreBytes = Default.Gauge("platform_codesynapse_store_bytes", "Bytes on disk per database and kind, where the database reports them, sampled daily.", "db_system", "kind")
)

// The db_system values of the store size gauges.
const (
	DBSystemNebula = "nebulagraph"
	DBSystemMilvus = "milvus"
)

var storeKinds = struct {
	sync.RWMutex
	m map[string]map[string]bool
}{m: map[string]map[string]bool{DBSystemPostgres: {}, DBSystemNebula: {}, DBSystemMilvus: {}}}

// AllowStoreKinds registers the kinds a store reports sizes for. Only the
// package that owns the schema calls it, with its own constant names.
func AllowStoreKinds(system string, kinds ...string) {
	storeKinds.Lock()
	defer storeKinds.Unlock()
	set, ok := storeKinds.m[system]
	if !ok {
		return
	}
	for _, k := range kinds {
		set[k] = true
	}
}

// StoreKindAllowed reports whether system and kind are in the fixed sets.
func StoreKindAllowed(system, kind string) bool {
	storeKinds.RLock()
	defer storeKinds.RUnlock()
	return storeKinds.m[system][kind]
}

// StoreSize is one sampled figure. Rows or bytes below 0 mean not reported.
type StoreSize struct {
	System string `json:"db_system"`
	Kind   string `json:"kind"`
	Rows   int64  `json:"rows"`
	Bytes  int64  `json:"bytes"`
}

// storeSeries remembers which series a sample set, so the next sample and
// ClearStoreSizes remove the ones it no longer reports.
var storeSeries = struct {
	sync.Mutex
	rows, bytes map[[2]string]bool
}{rows: map[[2]string]bool{}, bytes: map[[2]string]bool{}}

// SetStoreSizes publishes one sample in place of the last one. Sizes outside
// the registered sets are dropped, not echoed; it answers how many were kept.
func SetStoreSizes(ctx context.Context, sizes []StoreSize) int {
	storeSeries.Lock()
	defer storeSeries.Unlock()
	rows, bytes := map[[2]string]bool{}, map[[2]string]bool{}
	kept := 0
	Default.UpdateGauges(func() {
		for _, s := range sizes {
			if !StoreKindAllowed(s.System, s.Kind) || (s.Rows < 0 && s.Bytes < 0) {
				continue
			}
			k := [2]string{s.System, s.Kind}
			if s.Rows >= 0 {
				StoreRows.SetContext(ctx, float64(s.Rows), s.System, s.Kind)
				rows[k] = true
			}
			if s.Bytes >= 0 {
				StoreBytes.SetContext(ctx, float64(s.Bytes), s.System, s.Kind)
				bytes[k] = true
			}
			kept++
		}
		dropStale(ctx, StoreRows, storeSeries.rows, rows)
		dropStale(ctx, StoreBytes, storeSeries.bytes, bytes)
	})
	storeSeries.rows, storeSeries.bytes = rows, bytes
	return kept
}

// ClearStoreSizes removes every store size series, when this process stops
// sampling (the api-loops lease went elsewhere), so two processes never
// report the same figure.
func ClearStoreSizes(ctx context.Context) {
	SetStoreSizes(ctx, nil)
}

func dropStale(ctx context.Context, g *Gauge, before, now map[[2]string]bool) {
	for k := range before {
		if !now[k] {
			g.DeleteContext(ctx, k[0], k[1])
		}
	}
}
