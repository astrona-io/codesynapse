package vector_test

import (
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/vector"
	"github.com/parisnakitakejser/codesynapse/internal/vector/vectortest"
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
