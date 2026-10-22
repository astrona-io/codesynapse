package memory

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	agentmem "github.com/parisnakitakejser/codesynapse/internal/memory"
	"github.com/parisnakitakejser/codesynapse/internal/store"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func TestMemoryFull(t *testing.T) {
	ctx := context.Background()
	s := New()
	var batch []agentmem.Triple
	flush := func() error {
		b, err := agentmem.Prepare("r", batch, "t", time.Now())
		require.NoError(t, err)
		batch = nil
		return s.UpsertMemory(ctx, b)
	}
	for i := range agentmem.MaxFactsPerRepo {
		batch = append(batch, agentmem.Triple{Subject: fmt.Sprintf("s%d", i%100), Predicate: "P", Object: fmt.Sprintf("o%d", i/100)})
		if len(batch) == agentmem.MaxTriplesPerCall {
			require.NoError(t, flush())
		}
	}
	batch = []agentmem.Triple{{Subject: "s0", Predicate: "P", Object: "new"}}
	assert.True(t, errors.Is(flush(), store.ErrMemoryFull))
	g, err := s.SearchMemory(ctx, "r", "new", 10)
	require.NoError(t, err)
	assert.Equal(t, len(g.Entities), 0, "a rejected batch changes nothing")

	// Saving a known fact again still works when full.
	batch = []agentmem.Triple{{Subject: "s0", Predicate: "P", Object: "o0"}}
	require.NoError(t, flush())
	// Replacing frees room: s0's 100 P facts become one.
	batch = []agentmem.Triple{{Subject: "s0", Predicate: "P", Object: "new", Replace: true}}
	require.NoError(t, flush())
}
