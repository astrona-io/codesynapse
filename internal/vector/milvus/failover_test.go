package milvus

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/vector"
)

// TestClusterFailover runs against a Milvus cluster while pods are killed
// (just chart-failover-milvus, docs/SAAS.md 6d-2). It writes and searches
// without pause, runs CODESYNAPSE_TEST_MILVUS_FAILOVER (a shell command that
// kills pods) part way, and requires that operations recover and that every
// write reported as stored can be found afterwards.
func TestClusterFailover(t *testing.T) {
	addr, kill := os.Getenv("CODESYNAPSE_TEST_MILVUS_URL"), os.Getenv("CODESYNAPSE_TEST_MILVUS_FAILOVER")
	if addr == "" || kill == "" {
		t.Skip("CODESYNAPSE_TEST_MILVUS_URL and CODESYNAPSE_TEST_MILVUS_FAILOVER not set")
	}
	ctx := t.Context()
	idx, err := New(ctx, Config{URL: addr, Token: os.Getenv("CODESYNAPSE_TEST_MILVUS_TOKEN"), Prefix: "cs_ftest_"})
	require.NoError(t, err)
	require.NoError(t, idx.Drop(ctx))
	t.Cleanup(func() { _ = idx.Drop(context.Background()); _ = idx.Close(context.Background()) })

	const model, tenant, ns, dims = "failover", "t", "t|r", 16
	stored := map[string][]float32{}
	n := 0
	// op writes one new item and searches for it; it reports whether both
	// worked, and remembers the item only if Milvus confirmed the write.
	op := func() (bool, error) {
		n++
		id := fmt.Sprintf("e%05d", n)
		vec := make([]float32, dims)
		for i := range vec {
			vec[i] = rand.Float32()*2 - 1 //nolint:gosec // test vectors
		}
		opCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := idx.Upsert(opCtx, model, []vector.Item{{ID: id, Tenant: tenant, Namespace: ns, Vector: vec}}); err != nil {
			return false, err
		}
		stored[id] = vec
		hits, err := idx.Search(opCtx, model, tenant, ns, vec, 3)
		if err != nil {
			return false, err
		}
		if len(hits) == 0 || hits[0].ID != id {
			return false, fmt.Errorf("search for %s found %v", id, hits)
		}
		return true, nil
	}

	for range 20 {
		ok, err := op()
		require.True(t, ok, "before the failover: %v", err)
	}
	out, err := exec.CommandContext(ctx, "sh", "-c", kill).CombinedOutput() //nolint:gosec // the test's own setting
	require.NoError(t, err, string(out))
	killed := time.Now()
	t.Logf("killed: %s", out)

	failed, streak := 0, 0
	for streak < 20 {
		require.True(t, time.Since(killed) < 5*time.Minute, "operations did not recover within 5 minutes (%d failed)", failed)
		if ok, err := op(); ok {
			streak++
		} else {
			failed, streak = failed+1, 0
			t.Logf("%s after the kill: %v", time.Since(killed).Round(time.Second), err)
			time.Sleep(time.Second)
		}
	}
	t.Logf("recovered %s after the kill; %d calls retried, %d operations failed after their retries", time.Since(killed).Round(time.Second), idx.retries.Load(), failed)

	missing := 0
	for id, vec := range stored {
		hits, err := idx.Search(ctx, model, tenant, ns, vec, 3)
		require.NoError(t, err)
		if len(hits) == 0 || hits[0].ID != id {
			missing++
		}
	}
	assert.Equal(t, missing, 0, "every write Milvus confirmed is still there (%d confirmed)", len(stored))
}
