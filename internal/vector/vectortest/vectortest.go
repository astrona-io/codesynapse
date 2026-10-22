// Package vectortest is the contract every vector.Index passes.
package vectortest

import (
	"context"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
	"github.com/parisnakitakejser/codesynapse/internal/vector"
)

// Run exercises idx; it must start empty.
func Run(t *testing.T, idx vector.Index) {
	ctx := context.Background()
	const model = "test-embed"
	items := []vector.Item{
		{ID: "e-payments", Tenant: "org-a", Namespace: "org-a|repo", Vector: []float32{1, 0, 0}},
		{ID: "e-billing", Tenant: "org-a", Namespace: "org-a|repo", Vector: []float32{0.9, 0.1, 0}},
		{ID: "e-other-ns", Tenant: "org-a", Namespace: "org-a|other", Vector: []float32{1, 0, 0}},
		{ID: "e-tenant-b", Tenant: "org-b", Namespace: "org-b|repo", Vector: []float32{1, 0, 0}},
	}
	require.NoError(t, idx.Upsert(ctx, model, items))

	t.Run("nearest first, within tenant and namespace", func(t *testing.T) {
		hits, err := idx.Search(ctx, model, "org-a", "org-a|repo", []float32{1, 0, 0}, 10)
		require.NoError(t, err)
		require.Len(t, hits, 2)
		assert.Equal(t, hits[0].ID, "e-payments")
		assert.True(t, hits[0].Score > 0.99 && hits[0].Score <= 1.0001, "identical vectors score ~1")
		assert.Equal(t, hits[1].ID, "e-billing")
		for _, h := range hits {
			assert.Equal(t, h.Tenant, "org-a")
		}
	})

	t.Run("another tenant sees nothing of org-a", func(t *testing.T) {
		hits, err := idx.Search(ctx, model, "org-b", "org-a|repo", []float32{1, 0, 0}, 10)
		require.NoError(t, err)
		assert.Len(t, hits, 0, "a namespace of another tenant is not reachable by naming it")
		hits, err = idx.Search(ctx, model, "org-b", "org-b|repo", []float32{1, 0, 0}, 10)
		require.NoError(t, err)
		require.Len(t, hits, 1)
		assert.Equal(t, hits[0].ID, "e-tenant-b")
	})

	t.Run("tenant required", func(t *testing.T) {
		_, err := idx.Search(ctx, model, "", "org-a|repo", []float32{1, 0, 0}, 10)
		assert.Error(t, err)
		assert.Error(t, idx.Upsert(ctx, model, []vector.Item{{ID: "x", Namespace: "n", Vector: []float32{1}}}))
	})

	t.Run("other model or length", func(t *testing.T) {
		hits, err := idx.Search(ctx, "other-model", "org-a", "org-a|repo", []float32{1, 0, 0}, 10)
		require.NoError(t, err)
		assert.Len(t, hits, 0)
		hits, err = idx.Search(ctx, model, "org-a", "org-a|repo", []float32{1, 0}, 10)
		require.NoError(t, err)
		assert.Len(t, hits, 0)
	})

	t.Run("upsert replaces, delete removes", func(t *testing.T) {
		require.NoError(t, idx.Upsert(ctx, model, []vector.Item{{ID: "e-billing", Tenant: "org-a", Namespace: "org-a|repo", Vector: []float32{0, 0, 1}}}))
		hits, err := idx.Search(ctx, model, "org-a", "org-a|repo", []float32{0, 0, 1}, 1)
		require.NoError(t, err)
		require.True(t, len(hits) >= 1, "%v", hits)
		assert.Equal(t, hits[0].ID, "e-billing")
		require.NoError(t, idx.Delete(ctx, []string{"e-billing", "e-payments"}))
		hits, err = idx.Search(ctx, model, "org-a", "org-a|repo", []float32{1, 0, 0}, 10)
		require.NoError(t, err)
		assert.Len(t, hits, 0)
	})
}
