package semantic

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/vector"
)

func ids(t *testing.T, idx vector.Index, tenant, ns string) []string {
	t.Helper()
	var out []string
	after := ""
	for {
		page, err := idx.(vector.Sweeper).Scan(context.Background(), tenant, ns, after, vector.MaxScan)
		require.NoError(t, err)
		for _, it := range page {
			out = append(out, it.ID)
		}
		if len(page) < vector.MaxScan {
			return out
		}
		after = page[len(page)-1].ID
	}
}

func TestStale(t *testing.T) {
	scale := model.Node{ID: "r|Function|pkg.Scale", Kind: model.KindFunction, QualifiedName: "pkg.Scale", Summary: "scale workers"}
	file := model.Node{ID: "r|File|a.go", Kind: model.KindFile, QualifiedName: "a.go"}
	test := model.Node{ID: "r|Function|pkg.TestX", Kind: model.KindFunction, IsTest: true, QualifiedName: "pkg.TestX"}
	current, _ := itemID(scale.ID, Text(scale))
	live := Live([]model.Node{scale, file, test})
	assert.Equal(t, live, map[string]bool{current: true}, "only embeddable symbols, at their current description")

	edited := scale
	edited.Summary = "scale workers faster"
	old, _ := itemID(edited.ID, Text(edited))
	items := []vector.Item{{ID: current}, {ID: old}, {ID: "r|Function|pkg.Gone#0011223344556677"}}
	assert.Equal(t, Stale(items, live), []string{old, "r|Function|pkg.Gone#0011223344556677"})
	assert.Len(t, Stale(nil, live), 0)
	assert.Len(t, Stale([]vector.Item{{ID: current}}, live), 0)
}

func TestPruneRemovesStaleVectorsOfOneGraph(t *testing.T) {
	ctx := context.Background()
	idx := vector.NewFlat()
	st := Store{Idx: idx, Model: "m"}
	nodes := []model.Node{
		{ID: "r|Function|pkg.Scale", Kind: model.KindFunction, QualifiedName: "pkg.Scale", Summary: "scale workers"},
		{ID: "r|Function|pkg.Enqueue", Kind: model.KindFunction, QualifiedName: "pkg.Enqueue", Summary: "queue a job"},
	}
	_, err := st.Sync(ctx, fake{}, "r", nodes)
	require.NoError(t, err)
	// Symbols of another graph (node IDs carry their repository), and a
	// memory vector of another tenant under the same namespace name.
	_, err = st.Sync(ctx, fake{}, "other", []model.Node{
		{ID: "other|Function|pkg.Scale", Kind: model.KindFunction, QualifiedName: "pkg.Scale", Summary: "scale workers"},
		{ID: "other|Function|pkg.Enqueue", Kind: model.KindFunction, QualifiedName: "pkg.Enqueue", Summary: "queue a job"},
	})
	require.NoError(t, err)
	require.NoError(t, idx.Upsert(ctx, "m", []vector.Item{{ID: "memory-entity", Tenant: "org-a", Namespace: "r", Vector: []float32{1, 0, 0}}}))

	nodes[0].Summary = "scale workers on queued jobs" // a new description
	nodes = nodes[:1]                                 // Enqueue was removed
	_, err = st.Sync(ctx, fake{}, "r", nodes)
	require.NoError(t, err)
	assert.Len(t, ids(t, idx, Tenant, "r"), 3, "Sync only adds")

	got, err := st.Prune(ctx, "r", nodes)
	require.NoError(t, err)
	assert.Equal(t, got, PruneStats{Scanned: 3, Removed: 2, Complete: true})
	want, _ := itemID(nodes[0].ID, Text(nodes[0]))
	assert.Equal(t, ids(t, idx, Tenant, "r"), []string{want})
	assert.Len(t, ids(t, idx, Tenant, "other"), 2, "another graph keeps its vectors")
	assert.Equal(t, ids(t, idx, "org-a", ""), []string{"memory-entity"}, "another tenant keeps its vectors")

	again, err := st.Prune(ctx, "r", nodes)
	require.NoError(t, err)
	assert.Equal(t, again, PruneStats{Scanned: 1, Complete: true})
	_, err = st.Prune(ctx, "", nodes)
	assert.Error(t, err)
}

func TestPrunePagesThroughLargeGraphs(t *testing.T) {
	ctx := context.Background()
	idx := vector.NewFlat()
	var items []vector.Item
	for i := range scanPage*2 + 10 {
		items = append(items, vector.Item{ID: fmt.Sprintf("r|Function|f%05d#00", i), Tenant: Tenant, Namespace: "r", Vector: []float32{1}})
	}
	require.NoError(t, idx.Upsert(ctx, "m", items))
	got, err := Store{Idx: idx}.Prune(ctx, "r", nil)
	require.NoError(t, err)
	assert.Equal(t, got, PruneStats{Scanned: len(items), Removed: len(items), Complete: true})
	assert.Len(t, ids(t, idx, Tenant, "r"), 0)
}

func TestForgetAndSweep(t *testing.T) {
	ctx := context.Background()
	idx := vector.NewFlat()
	st := Store{Idx: idx, Model: "m"}
	for _, g := range []string{"kept", "gone-1", "gone-2", "deleted"} {
		nodes := []model.Node{{ID: g + "|Function|pkg.F", Kind: model.KindFunction, QualifiedName: "pkg.F"}}
		_, err := st.Sync(ctx, fake{}, g, nodes)
		require.NoError(t, err)
	}
	require.NoError(t, idx.Upsert(ctx, "m", []vector.Item{{ID: "memory-entity", Tenant: "org-a", Namespace: "gone-1", Vector: []float32{1, 0, 0}}}))

	require.NoError(t, st.Forget(ctx, "deleted"))
	assert.Len(t, ids(t, idx, Tenant, "deleted"), 0)
	assert.Error(t, st.Forget(ctx, ""))

	asked := map[string]int{}
	exists := func(_ context.Context, g string) (bool, error) {
		asked[g]++
		return g == "kept", nil
	}
	got, err := st.Sweep(ctx, "", 5, exists)
	require.NoError(t, err)
	assert.Equal(t, got, SweepStats{Scanned: 3, Graphs: 2})
	assert.Equal(t, asked, map[string]int{"kept": 1, "gone-1": 1, "gone-2": 1})
	assert.Len(t, ids(t, idx, Tenant, "kept"), 1)
	assert.Len(t, ids(t, idx, Tenant, "gone-1"), 0)
	assert.Equal(t, ids(t, idx, "org-a", ""), []string{"memory-entity"}, "only the code-graph tenant is swept")

	_, err = st.Sweep(ctx, "", 5, func(context.Context, string) (bool, error) { return false, errors.New("store down") })
	assert.Error(t, err)
	assert.Len(t, ids(t, idx, Tenant, "kept"), 1, "an unanswered check deletes nothing")

	_, err = Store{Idx: noSweep{idx}}.Prune(ctx, "kept", nil)
	assert.True(t, errors.Is(err, ErrNoSweeper))
}

func TestSweepResumesWhereItStopped(t *testing.T) {
	ctx := context.Background()
	idx := vector.NewFlat()
	var items []vector.Item
	for i := range scanPage + 5 {
		items = append(items, vector.Item{ID: fmt.Sprintf("g%05d#00", i), Tenant: Tenant, Namespace: fmt.Sprintf("g%d", i%3), Vector: []float32{1}})
	}
	require.NoError(t, idx.Upsert(ctx, "m", items))
	st := Store{Idx: idx}
	live := func(_ context.Context, g string) (bool, error) { return g != "g1", nil }
	first, err := st.Sweep(ctx, "", 1, live)
	require.NoError(t, err)
	assert.Equal(t, first.Scanned, scanPage)
	assert.Equal(t, first.Next, fmt.Sprintf("g%05d#00", scanPage-1))
	second, err := st.Sweep(ctx, first.Next, 1, live)
	require.NoError(t, err)
	assert.Equal(t, second.Next, "", "a full pass starts over")
	assert.Len(t, ids(t, idx, Tenant, "g1"), 0)
	assert.Len(t, ids(t, idx, Tenant, "g0"), (scanPage+5+2)/3)
}

// noSweep hides the Sweeper methods of an index.
type noSweep struct{ vector.Index }
