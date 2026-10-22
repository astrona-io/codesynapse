package metrics

import (
	"context"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
)

func TestDistribute(t *testing.T) {
	d := Distribute([]int{1, 1, 2, 3, 8, 30}, []int{1, 2, 5, 10})
	assert.Equal(t, d.Count, 6)
	assert.Equal(t, d.Min, 1)
	assert.Equal(t, d.Max, 30)
	assert.Equal(t, d.P50, 2)
	assert.Equal(t, d.P90, 30)
	labels := []string{}
	counts := []int{}
	for _, b := range d.Buckets {
		labels = append(labels, b.Label)
		counts = append(counts, b.Count)
	}
	assert.Equal(t, labels, []string{"≤1", "2", "3–5", "6–10", ">10"})
	assert.Equal(t, counts, []int{2, 1, 1, 1, 1})

	empty := Distribute(nil, []int{1})
	assert.Equal(t, empty.Count, 0)
	assert.Len(t, empty.Buckets, 2)
}

func TestCompute(t *testing.T) {
	g := storetest.Fixture()
	// Give the files sizes so the benchmark has a baseline.
	for _, f := range []string{"app/app.go", "app/app_test.go"} {
		n, ok := g.Node(storetest.ID(model.KindFile, f))
		require.True(t, ok)
		n.Bytes = 4000
	}
	// An unexported function nobody calls is a dead-code candidate.
	g.AddNode(model.Node{ID: storetest.ID(model.KindFunction, "app.unused"), Kind: model.KindFunction, Repo: storetest.Repo,
		Name: "unused", QualifiedName: "app.unused", File: "app/app.go", StartLine: 40, EndLine: 44, LOC: 5})

	rep, err := Compute(context.Background(), storetest.Repo, g, Options{})
	require.NoError(t, err)

	assert.Equal(t, rep.Tests.TestFunctions, 1)
	// TestHandler -> Handler -> (I).Do -> (*impl).Do: Handler and impl.Do are callables reached.
	assert.Equal(t, rep.Tests.Total, 4)
	assert.Equal(t, rep.Tests.Reachable, 2)
	assert.Equal(t, rep.Tests.ReachablePct, 50.0)
	assert.Equal(t, rep.Code.SecuritySymbols, 1)

	names := map[string]bool{}
	for _, d := range rep.DeadCode {
		names[d.Name] = true
	}
	assert.True(t, names["app.unused"])
	assert.False(t, names["app.main"], "main is an entry point")

	assert.Greater(t, rep.Benchmark.Samples, 0)
	assert.Greater(t, rep.Benchmark.MedianSavingsX, 1.0)
	assert.Greater(t, rep.Benchmark.MedianReductionPct, 0.0)
	assert.LessOrEqual(t, rep.Benchmark.TotalReductionPct, 100.0)
	assert.Equal(t, rep.Distributions["complexity"].Count, 4)

	enc, err := Encode(rep)
	require.NoError(t, err)
	assert.Contains(t, enc, `"median_savings_x"`)
}

func TestReduction(t *testing.T) {
	assert.Equal(t, reduction(50, 1000), 95.0)
	assert.Equal(t, reduction(1, 3), 66.7)
	assert.Equal(t, reduction(2000, 1000), 0.0, "never negative")
	assert.Equal(t, reduction(10, 0), 0.0)
}
