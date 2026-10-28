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
	r, _, ctx := compressRegistry(t, new(false), local)
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

func TestGatewayTargetNeedsOptInAndLocal(t *testing.T) {
	local := ProviderConfig{Name: "l", Kind: "openai", BaseURL: "http://localhost:11434/v1", Model: "m", Gateway: true}
	cloud := ProviderConfig{Name: "c", Kind: "openai", BaseURL: "https://api.example.com/v1", Model: "m", Gateway: true}
	r := NewRegistry(Config{Providers: []ProviderConfig{cloud}})
	_, ok := r.GatewayTarget()
	assert.True(t, !ok, "a cloud provider is never a gateway target")
	local.Gateway = false
	r = NewRegistry(Config{Providers: []ProviderConfig{local}})
	_, ok = r.GatewayTarget()
	assert.True(t, !ok, "off unless the provider opts in")
	local.Gateway = true
	r = NewRegistry(Config{Providers: []ProviderConfig{local}})
	got, ok := r.GatewayTarget()
	assert.True(t, ok)
	assert.Equal(t, got.Model, "m")
}
