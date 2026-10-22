package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/astrona-io/codesynapse/internal/logx"
	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// Reserve another generation of slots for zero observations after deletion.
const memoryMetricTenants = 2048

type memoryFactCounter interface {
	MemoryFactCounts(context.Context, string, int) ([]store.MemoryFactCount, error)
}

// collectMemoryFacts publishes nothing until every namespace page succeeded.
// The deadline bounds enumeration; the tenant budget bounds aggregation memory.
func collectMemoryFacts(ctx context.Context, st memoryFactCounter) (map[string]int64, error) {
	counts := map[string]int64{}
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := st.MemoryFactCounts(ctx, after, 100)
		if err != nil {
			return nil, fmt.Errorf("counting durable memory: %w", err)
		}
		if len(page) == 0 {
			return counts, nil
		}
		if len(page) > 100 {
			return nil, errors.New("memory count page exceeds limit")
		}
		for _, row := range page {
			if row.Namespace <= after || row.Facts < 0 {
				return nil, errors.New("invalid memory count page")
			}
			after = row.Namespace
			tenant := memory.OrgOf(row.Namespace)
			if tenant == "" || telemetry.TenantLabel(tenant) != tenant {
				continue
			}
			counts[tenant] += row.Facts
			if len(counts) > memoryMetricTenants {
				return nil, errors.New("memory count tenant budget exceeded")
			}
		}
	}
}

// observeMemoryFacts runs in each API replica. Panels select the newest fresh
// observation per tenant and use max for replicas with the same timestamp.
func (s *Server) observeMemoryFacts(ctx context.Context) {
	observations := memoryFactObservations{}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		readCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		counts, err := collectMemoryFacts(readCtx, s.st)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			telemetry.MemoryFactsCollectionErrors.IncContext(ctx)
			s.logger.Warn("durable memory metrics are stale", logx.Error(err))
		} else {
			observations.observe(ctx, counts, float64(time.Now().Unix()))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type memoryFactObservations struct {
	previous map[string]int64
	retired  map[string]bool
}

func (o *memoryFactObservations) observe(ctx context.Context, counts map[string]int64, at float64) {
	telemetry.Default.UpdateGauges(func() {
		// Keep one fresh zero after removal, then release local series slots.
		for tenant := range o.retired {
			if _, ok := counts[tenant]; !ok {
				tenantCtx := memoryMetricContext(ctx, tenant)
				telemetry.MemoryFactsCurrent.DeleteContext(tenantCtx, tenant)
				telemetry.MemoryFactsObservedAt.DeleteContext(tenantCtx, tenant)
			}
		}
		o.retired = map[string]bool{}
		for tenant := range o.previous {
			if _, ok := counts[tenant]; !ok {
				publishMemoryFacts(ctx, tenant, 0, at)
				o.retired[tenant] = true
			}
		}
		for tenant, n := range counts {
			publishMemoryFacts(ctx, tenant, n, at)
		}
		o.previous = counts
	})
}

func publishMemoryFacts(ctx context.Context, tenant string, count int64, at float64) {
	ctx = memoryMetricContext(ctx, tenant)
	telemetry.MemoryFactsCurrent.SetContext(ctx, float64(count), tenant)
	telemetry.MemoryFactsObservedAt.SetContext(ctx, at, tenant)
}

func memoryMetricContext(ctx context.Context, tenant string) context.Context {
	return telemetry.WithModel(telemetry.WithTool(telemetry.WithTenant(ctx, tenant), "none"), "none", "none")
}
