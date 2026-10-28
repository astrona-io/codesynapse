package llm

import (
	"context"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

type embedStub struct{ stubProvider }

func (*embedStub) Embed(_ context.Context, _ string, in []string) ([][]float32, error) {
	return make([][]float32, len(in)), nil
}

func TestEmbedWaitsForTheProviderSlot(t *testing.T) {
	r, _, ctx := compressRegistry(t, new(false), &stubProvider{text: func(Request) string { return "ok" }})
	r.mu.Lock()
	for i := range r.cfg.Providers {
		if r.cfg.Providers[i].Name == "local" {
			r.cfg.Providers[i].EmbeddingModel = "emb"
		}
	}
	r.mu.Unlock()
	r.impl["local"] = &embedStub{}
	release, err := r.slots["local"].acquire(ctx)
	require.NoError(t, err)
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	_, err = r.Embed(short, "local", []string{"a"})
	assert.ErrorIs(t, err, context.DeadlineExceeded, "queued behind a running completion")
	release()
	vecs, err := r.Embed(ctx, "local", []string{"a"})
	require.NoError(t, err)
	assert.Equal(t, len(vecs), 1)
}
