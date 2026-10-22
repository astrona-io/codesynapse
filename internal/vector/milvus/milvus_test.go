package milvus

import (
	"context"
	"os"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/vector"
	"github.com/astrona-io/codesynapse/internal/vector/vectortest"
)

// TestContract runs against a real Milvus when CODESYNAPSE_TEST_MILVUS_URL
// is set (just test-milvus). It drops its own collections only.
func TestContract(t *testing.T) {
	addr := os.Getenv("CODESYNAPSE_TEST_MILVUS_URL")
	if addr == "" {
		t.Skip("CODESYNAPSE_TEST_MILVUS_URL not set")
	}
	ctx := context.Background()
	idx, err := New(ctx, Config{URL: addr, Token: os.Getenv("CODESYNAPSE_TEST_MILVUS_TOKEN"), Prefix: "cs_vtest_"})
	require.NoError(t, err)
	require.NoError(t, idx.Drop(ctx))
	t.Cleanup(func() { _ = idx.Drop(ctx); _ = idx.Close(ctx) })
	vectortest.Run(t, idx)
}

// TestRecoversFromDroppedCollection: a collection dropped or released behind
// the index's back (Milvus restart, operator) must not break it until restart.
func TestRecoversFromDroppedCollection(t *testing.T) {
	addr := os.Getenv("CODESYNAPSE_TEST_MILVUS_URL")
	if addr == "" {
		t.Skip("CODESYNAPSE_TEST_MILVUS_URL not set")
	}
	ctx := context.Background()
	idx, err := New(ctx, Config{URL: addr, Prefix: "cs_rtest_"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = idx.Drop(ctx); _ = idx.Close(ctx) })
	item := []vector.Item{{ID: "e1", Tenant: "t", Namespace: "t|r", Vector: []float32{1, 0}}}
	require.NoError(t, idx.Upsert(ctx, "m", item))

	other, err := New(ctx, Config{URL: addr, Prefix: "cs_rtest_"})
	require.NoError(t, err)
	require.NoError(t, other.Drop(ctx)) // behind idx's back

	hits, err := idx.Search(ctx, "m", "t", "t|r", []float32{1, 0}, 5)
	require.NoError(t, err)
	assert.Len(t, hits, 0, "a dropped collection holds nothing")
	require.NoError(t, idx.Upsert(ctx, "m", item), "the collection is recreated")
	hits, err = idx.Search(ctx, "m", "t", "t|r", []float32{1, 0}, 5)
	require.NoError(t, err)
	assert.Len(t, hits, 1)
}
