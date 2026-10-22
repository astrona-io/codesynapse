package vector_test

import (
	"math"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/vector"
	"github.com/astrona-io/codesynapse/internal/vector/vectortest"
)

func TestFlatContract(t *testing.T) { vectortest.Run(t, vector.NewFlat()) }

func TestIsolateDropsForeignHits(t *testing.T) {
	hits := []vector.Hit{
		{ID: "a", Tenant: "t1", Namespace: "t1|r", Score: 0.5},
		{ID: "b", Tenant: "t2", Namespace: "t1|r", Score: 0.9},
		{ID: "c", Tenant: "t1", Namespace: "t1|other", Score: 0.9},
		{ID: "d", Tenant: "t1", Namespace: "t1|r", Score: 0.7},
	}
	got := vector.Isolate(hits, "t1", "t1|r", 10)
	assert.Equal(t, len(got), 2)
	assert.Equal(t, got[0].ID, "d")
	assert.Equal(t, got[1].ID, "a")
}

func TestIsolateRejectsMalformedAndDuplicateCandidates(t *testing.T) {
	var hits []vector.Hit
	for _, item := range []struct {
		id    string
		score float64
	}{
		{"same", .8}, {"same", .9}, {"", .99}, {"nan", math.NaN()},
		{"inf", math.Inf(1)}, {"negative", -.1}, {"outside", 1.1},
	} {
		hits = append(hits, vector.Hit{ID: item.id, Score: item.score, Tenant: "t", Namespace: "t|r"})
	}
	got := vector.Isolate(hits, "t", "t|r", 10)
	assert.Equal(t, len(got), 1)
	assert.Equal(t, got[0].ID, "same")
	assert.Equal(t, got[0].Score, .9)
	assert.Len(t, vector.Isolate(got, "t", "t|r", -1), 0)
	rounded := vector.Isolate([]vector.Hit{{ID: "identical", Tenant: "t", Namespace: "t|r", Score: 1.00000001}}, "t", "t|r", 10)
	assert.Equal(t, rounded[0].Score, 1.0)
}

func TestClampK(t *testing.T) {
	for k, want := range map[int]int{-1: vector.DefaultK, 0: vector.DefaultK, 1: vector.MinK, 5: 5, 50: vector.MaxK} {
		assert.Equal(t, vector.ClampK(k), want, k)
	}
}
