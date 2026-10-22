package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/store"
	"github.com/parisnakitakejser/codesynapse/internal/telemetry"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

type memoryCountPages struct {
	rows      []store.MemoryFactCount
	failAfter string
	queries   int
}

func (p *memoryCountPages) MemoryFactCounts(_ context.Context, after string, limit int) ([]store.MemoryFactCount, error) {
	p.queries++
	if p.failAfter != "" && after >= p.failAfter {
		return nil, errors.New("database unavailable")
	}
	for i, row := range p.rows {
		if row.Namespace > after {
			return p.rows[i:min(i+limit, len(p.rows))], nil
		}
	}
	return nil, nil
}

func TestMemoryCountsCompleteAndTenantScoped(t *testing.T) {
	a, b := strings.Repeat("a", 24), strings.Repeat("b", 24)
	pages := &memoryCountPages{}
	for i := range 205 {
		pages.rows = append(pages.rows, store.MemoryFactCount{Namespace: fmt.Sprintf("%s|repo-%03d", a, i), Facts: 2})
	}
	pages.rows = append(pages.rows, store.MemoryFactCount{Namespace: b + "|repo", Facts: 3}, store.MemoryFactCount{Namespace: "legacy", Facts: 100}, store.MemoryFactCount{Namespace: "untrusted|repo", Facts: 500})
	counts, err := collectMemoryFacts(context.Background(), pages)
	require.NoError(t, err)
	assert.Equal(t, counts, map[string]int64{a: 410, b: 3})
	assert.Equal(t, pages.queries, 4, "read the terminating page; never silently truncate tenants")
	pages.failAfter = pages.rows[100].Namespace
	counts, err = collectMemoryFacts(context.Background(), pages)
	require.Error(t, err)
	assert.Equal(t, counts, map[string]int64(nil), "a failed later page must not replace the previous complete observation")
}

func TestMemoryCountsBudgetAndCancellation(t *testing.T) {
	pages := &memoryCountPages{}
	for i := range memoryMetricTenants + 1 {
		pages.rows = append(pages.rows, store.MemoryFactCount{Namespace: fmt.Sprintf("%024x|repo", i), Facts: 1})
	}
	counts, err := collectMemoryFacts(context.Background(), pages)
	require.Error(t, err)
	assert.Equal(t, counts, map[string]int64(nil))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = collectMemoryFacts(ctx, pages)
	assert.ErrorIs(t, err, context.Canceled)
}

type invalidMemoryPage struct{ rows []store.MemoryFactCount }

func (p invalidMemoryPage) MemoryFactCounts(context.Context, string, int) ([]store.MemoryFactCount, error) {
	return p.rows, nil
}

func TestMemoryCountsRejectInvalidPage(t *testing.T) {
	for name, rows := range map[string][]store.MemoryFactCount{
		"no progress":        {{Namespace: ""}},
		"repeated namespace": {{Namespace: "a"}, {Namespace: "a"}},
		"negative count":     {{Namespace: "a", Facts: -1}},
		"oversized page":     slices.Repeat([]store.MemoryFactCount{{Namespace: "a"}}, 101),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := collectMemoryFacts(context.Background(), invalidMemoryPage{rows: rows})
			require.Error(t, err)
		})
	}
}

func TestMemoryFactsObservationsReflectDeletionAndReleaseSeries(t *testing.T) {
	const tenant = "eeeeeeeeeeeeeeeeeeeeeeee"
	ctx := context.Background()
	telemetry.AllowTools("memory-metric-test")
	telemetry.AllowModels("memory-metric-test", "model")
	ctx = telemetry.WithModel(telemetry.WithTool(ctx, "memory-metric-test"), "memory-metric-test", "model")
	state := memoryFactObservations{}
	read := func() string {
		t.Helper()
		var out strings.Builder
		require.NoError(t, telemetry.Default.WriteText(&out))
		return out.String()
	}
	labels := `{tenant_id="` + tenant + `",mcp_tool="none",llm_provider="none",llm_model="none"}`
	state.observe(ctx, map[string]int64{tenant: 7}, 100)
	assert.Contains(t, read(), "agent_memory_facts_count"+labels+" 7")
	state.observe(ctx, map[string]int64{tenant: 2}, 130)
	assert.Contains(t, read(), "agent_memory_facts_count"+labels+" 2")
	state.observe(ctx, map[string]int64{}, 160)
	out := read()
	assert.Contains(t, out, "agent_memory_facts_count"+labels+" 0")
	assert.Contains(t, out, "agent_memory_facts_observed_at_seconds"+labels+" 160")
	state.observe(ctx, map[string]int64{}, 190)
	assert.False(t, strings.Contains(read(), "agent_memory_facts_count"+labels), "retired tenant releases bounded local series slots")
}
