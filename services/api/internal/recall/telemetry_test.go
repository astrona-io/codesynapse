package recall

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/memory"
	"github.com/parisnakitakejser/codesynapse/internal/store"
	"github.com/parisnakitakejser/codesynapse/internal/telemetry"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

type unavailableRecallStore struct{ store.MemoryStore }

func (unavailableRecallStore) MemoryLexical(context.Context, string, string, int) ([]memory.Scored, error) {
	return nil, errors.New("database unavailable")
}

func recallCount(t *testing.T, tenant string) float64 {
	t.Helper()
	var out strings.Builder
	require.NoError(t, telemetry.Default.WriteText(&out))
	var total float64
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "agent_memory_recalls_total{") && strings.Contains(line, `tenant_id="`+tenant+`"`) {
			v, err := strconv.ParseFloat(strings.Fields(line)[1], 64)
			require.NoError(t, err)
			total += v
		}
	}
	return total
}

func TestLogicalRecallCountIncludesGateAndFailureOnce(t *testing.T) {
	const tenant = "cccccccccccccccccccccccc"
	s, _ := setup(t, true)
	before := recallCount(t, tenant)
	_, err := s.Recall(t.Context(), tenant+"|r", Query{Text: "payments ownership"})
	require.NoError(t, err)
	assert.Equal(t, recallCount(t, tenant)-before, float64(1), "hybrid legs are one logical recall")
	_, err = s.Recall(t.Context(), tenant+"|r", Query{Text: "thanks!"})
	require.NoError(t, err)
	assert.Equal(t, recallCount(t, tenant)-before, float64(2), "intent-gated request is counted once")
	s.St = unavailableRecallStore{s.St}
	_, err = s.Recall(t.Context(), tenant+"|r", Query{Text: "payments ownership"})
	require.Error(t, err)
	assert.Equal(t, recallCount(t, tenant)-before, float64(3), "an accepted request remains a recall when its backend fails")
}

type tenantEmbeddingStore struct {
	store.MemoryStore
	t       *testing.T
	tenants map[string]int
}

func (s tenantEmbeddingStore) SetMemoryEmbeddings(ctx context.Context, model string, vectors map[string][]float32) error {
	s.t.Helper()
	tenant := telemetry.Tenant(ctx)
	s.tenants[tenant] += len(vectors)
	for id := range vectors {
		valid := false
		for _, name := range []string{"Service", "Team"} {
			valid = valid || id == memory.EntityID(tenant+"|repo", name)
		}
		assert.True(s.t, valid, "a batch must not attribute another tenant's entity to %s", tenant)
	}
	return s.MemoryStore.SetMemoryEmbeddings(ctx, model, vectors)
}

func TestBackgroundEmbeddingsAttributeEachTenant(t *testing.T) {
	s, st := setup(t, true)
	// Remove the setup fixture so only these two organisations remain pending.
	for _, name := range []string{"Payments service", "Team Blue", "Small PRs", "Frontend", "Vue"} {
		require.NoError(t, st.DeleteMemoryEntity(t.Context(), "r", memory.EntityID("r", name)))
	}
	a, b := strings.Repeat("a", 24), strings.Repeat("b", 24)
	for _, tenant := range []string{a, b} {
		batch, err := memory.Prepare(tenant+"|repo", []memory.Triple{{Subject: "Service", Predicate: "OWNED_BY", Object: "Team"}}, "test", time.Now())
		require.NoError(t, err)
		require.NoError(t, st.UpsertMemory(t.Context(), batch))
	}
	seen := map[string]int{}
	s.St = tenantEmbeddingStore{MemoryStore: st, t: t, tenants: seen}
	n, err := s.EmbedPending(t.Context())
	require.NoError(t, err)
	assert.Equal(t, n, 4)
	assert.Equal(t, seen, map[string]int{a: 2, b: 2})
}
