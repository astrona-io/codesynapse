package llm

import (
	"context"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// queued starts a request that waits for s and sends its name with its
// release once it holds the slot; it returns when the request is queued.
func queued(t *testing.T, s *slots, name string, first bool, got chan<- func() string) {
	t.Helper()
	_, before := s.counts()
	go func() {
		take := s.acquire
		if first {
			take = s.acquireFirst
		}
		release, err := take(context.Background())
		if err != nil {
			t.Error(err)
			return
		}
		got <- func() string { release(); return name }
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, waiting := s.counts(); waiting > before {
			return
		}
		require.True(t, time.Now().Before(deadline), "request never queued")
		time.Sleep(time.Millisecond)
	}
}

func TestQuickRequestsGoFirst(t *testing.T) {
	s := newSlots(ProviderConfig{BaseURL: "http://127.0.0.1:1/v1"})
	release, err := s.acquire(context.Background())
	require.NoError(t, err)
	got := make(chan func() string, 4)
	queued(t, s, "large", false, got)
	queued(t, s, "quick-1", true, got)
	queued(t, s, "quick-2", true, got)
	release()
	var order []string
	for range 3 {
		order = append(order, (<-got)())
	}
	assert.Equal(t, order, []string{"quick-1", "quick-2", "large"})
	running, waiting := s.counts()
	assert.Equal(t, running, 0)
	assert.Equal(t, waiting, 0)
}

// TestQuickRequestsDoNotStarveLargeOnes keeps a quick request waiting at
// every release: the large one still gets the slot after maxPassed of them.
func TestQuickRequestsDoNotStarveLargeOnes(t *testing.T) {
	s := newSlots(ProviderConfig{BaseURL: "http://127.0.0.1:1/v1"})
	release, err := s.acquire(context.Background())
	require.NoError(t, err)
	got := make(chan func() string, 1)
	queued(t, s, "large", false, got)
	queued(t, s, "quick", true, got)
	release()
	var order []string
	for range maxPassed + 1 {
		finish := <-got
		if len(order) < maxPassed {
			queued(t, s, "quick", true, got)
		}
		order = append(order, finish())
	}
	assert.Equal(t, order[maxPassed-1], "quick")
	assert.Equal(t, order[maxPassed], "large", "the large request is served after maxPassed quick ones")
	(<-got)()
	running, waiting := s.counts()
	assert.Equal(t, running, 0)
	assert.Equal(t, waiting, 0)
}

// TestCancelledWaiterHandsItsSlotOn leaves no slot behind when a waiter
// gives up, also when it is the one picked next.
func TestCancelledWaiterHandsItsSlotOn(t *testing.T) {
	s := newSlots(ProviderConfig{BaseURL: "http://127.0.0.1:1/v1"})
	release, err := s.acquire(context.Background())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	gave := make(chan error, 1)
	go func() {
		_, err := s.acquireFirst(ctx)
		gave <- err
	}()
	for {
		if _, waiting := s.counts(); waiting == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	got := make(chan func() string, 1)
	queued(t, s, "large", false, got)
	cancel()
	assert.ErrorIs(t, <-gave, context.Canceled)
	release()
	assert.Equal(t, (<-got)(), "large")
	running, waiting := s.counts()
	assert.Equal(t, running, 0)
	assert.Equal(t, waiting, 0)
}
