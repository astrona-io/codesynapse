package llm

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestLocalProviderRunsOneRequestAtATime(t *testing.T) {
	var now, peak atomic.Int32
	local := &stubProvider{text: func(Request) string {
		n := now.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		now.Add(-1)
		return "ok"
	}}
	r, _, ctx := compressRegistry(t, false, local)
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.Complete(ctx, "local", Request{System: "s", User: "u"})
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	assert.Equal(t, int(peak.Load()), 1)
	assert.Equal(t, len(local.got), 5, "every queued request is answered")
}

func TestQueuedRequestStopsWhenContextEnds(t *testing.T) {
	s := newSlots(ProviderConfig{BaseURL: "http://127.0.0.1:1/v1"})
	release, err := s.acquire(context.Background())
	require.NoError(t, err)
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = s.acquire(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.True(t, newSlots(ProviderConfig{BaseURL: "https://api.example.com/v1"}) == nil, "cloud is unlimited by default")
}
