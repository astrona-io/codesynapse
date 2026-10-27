package llm

import (
	"context"
	"sync/atomic"
)

// slots queues requests for one provider: at most cap run, the others wait.
// A nil *slots (provider added after start) never limits.
type slots struct {
	ch      chan struct{}
	waiting atomic.Int32
}

func newSlots(p ProviderConfig) *slots {
	n := p.MaxConcurrent
	if n <= 0 && p.Local() {
		n = 1
	}
	if n <= 0 {
		return nil
	}
	return &slots{ch: make(chan struct{}, n)}
}

// acquire waits for a free slot or the end of ctx and returns the release.
func (s *slots) acquire(ctx context.Context) (func(), error) {
	if s == nil {
		return func() {}, nil
	}
	select {
	case s.ch <- struct{}{}:
		return func() { <-s.ch }, nil
	default:
	}
	s.waiting.Add(1)
	defer s.waiting.Add(-1)
	select {
	case s.ch <- struct{}{}:
		return func() { <-s.ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *slots) counts() (running, waiting int) {
	if s == nil {
		return 0, 0
	}
	return len(s.ch), int(s.waiting.Load())
}
