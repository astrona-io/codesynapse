package llm

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// gatewayTask labels the queue waits of agent gateway traffic, which runs
// under no task of its own.
const gatewayTask = "gateway"

// maxPassed is how many requests that asked to go first may overtake a
// waiting request before it gets the next free slot, so a steady stream of
// quick requests delays a large one by a bounded number of answers and
// never starves it.
const maxPassed = 4

// slots queues requests for one provider: at most size run, the others wait
// in order, except that a request taken with acquireFirst is served before
// the ordinary ones waiting ahead of it (at most maxPassed times each).
// A nil *slots (provider added after start, or a cloud provider without
// max_concurrent) never limits and records nothing.
type slots struct {
	size          int
	provider      string
	model         string
	capacity      sync.Once
	qmu           sync.Mutex
	running       int
	queue         []*waiter
	mu            sync.Mutex
	tenantRunning map[string]int
	tenantWaiting map[string]int
}

// waiter is one request in the queue; ready is closed when it holds a slot.
type waiter struct {
	ready  chan struct{}
	first  bool
	passed int
}

// QueueSlots is how many requests a provider runs at once in each process:
// max_concurrent, one for a local model without it, or 0 for unlimited.
func (p ProviderConfig) QueueSlots() int {
	if p.MaxConcurrent <= 0 && p.Local() {
		return 1
	}
	return max(p.MaxConcurrent, 0)
}

func newSlots(p ProviderConfig) *slots {
	n := p.QueueSlots()
	if n == 0 {
		return nil
	}
	return &slots{size: n, provider: p.Name, model: p.Model,
		tenantRunning: map[string]int{}, tenantWaiting: map[string]int{}}
}

// acquire waits for a free slot in turn or the end of ctx and returns the
// release.
func (s *slots) acquire(ctx context.Context) (func(), error) { return s.take(ctx, false) }

// acquireFirst waits like acquire but ahead of the ordinary requests: for
// short requests that finish fast.
func (s *slots) acquireFirst(ctx context.Context) (func(), error) { return s.take(ctx, true) }

// take waits for a slot. Every wait is recorded, also one that found a slot
// at once or ended before one came free, so the statistics count every
// request.
func (s *slots) take(ctx context.Context, first bool) (func(), error) {
	if s == nil {
		return func() {}, nil
	}
	mctx := telemetry.WithTool(telemetry.WithModel(ctx, s.provider, s.model), "none")
	// Published with the first request, under its context: building the
	// registry has none. An idle process reports no capacity yet.
	s.capacity.Do(func() { telemetry.SetQueueCapacity(mctx, s.provider, s.size) })
	task := queueTask(ctx)
	start := time.Now()
	s.qmu.Lock()
	if s.running < s.size && len(s.queue) == 0 {
		s.running++
		s.qmu.Unlock()
		s.load(mctx, 1, 0)
		telemetry.RecordQueueWait(mctx, task, telemetry.QueueAcquired, 0)
		return s.release(mctx), nil
	}
	w := &waiter{ready: make(chan struct{}), first: first}
	s.queue = append(s.queue, w)
	s.qmu.Unlock()
	s.load(mctx, 0, 1)
	select {
	case <-w.ready:
		s.load(mctx, 1, -1)
		telemetry.RecordQueueWait(mctx, task, telemetry.QueueAcquired, time.Since(start))
		return s.release(mctx), nil
	case <-ctx.Done():
		s.qmu.Lock()
		if i := slices.Index(s.queue, w); i >= 0 {
			s.queue = slices.Delete(s.queue, i, i+1)
		} else {
			// The slot came free as ctx ended: hand it on.
			s.running--
			s.grant()
		}
		s.qmu.Unlock()
		s.load(mctx, 0, -1)
		outcome := telemetry.QueueCancelled
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			outcome = telemetry.QueueTimeout
		}
		telemetry.RecordQueueWait(mctx, task, outcome, time.Since(start))
		return nil, ctx.Err()
	}
}

// grant gives free slots to the next waiters; the caller holds qmu.
func (s *slots) grant() {
	for s.running < s.size && len(s.queue) > 0 {
		i := s.next()
		w := s.queue[i]
		s.queue = slices.Delete(s.queue, i, i+1)
		s.running++
		close(w.ready)
	}
}

// next picks the waiter to serve: the oldest one that asked to go first,
// unless an ordinary one ahead of it was passed over maxPassed times already.
func (s *slots) next() int {
	j := slices.IndexFunc(s.queue, func(w *waiter) bool { return w.first })
	if j <= 0 || s.queue[0].passed >= maxPassed {
		return 0
	}
	for _, w := range s.queue[:j] {
		w.passed++
	}
	return j
}

func (s *slots) release(mctx context.Context) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			s.qmu.Lock()
			s.running--
			s.grant()
			s.qmu.Unlock()
			s.load(mctx, -1, 0)
		})
	}
}

// load moves the tenant's running and waiting counts in this process and
// publishes them; a tenant that goes quiet keeps its series at zero.
func (s *slots) load(mctx context.Context, running, waiting int) {
	tenant := telemetry.Tenant(mctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tenantRunning[tenant] += running
	s.tenantWaiting[tenant] += waiting
	telemetry.SetQueueLoad(mctx, s.tenantRunning[tenant], s.tenantWaiting[tenant])
}

func (s *slots) counts() (running, waiting int) {
	if s == nil {
		return 0, 0
	}
	s.qmu.Lock()
	defer s.qmu.Unlock()
	return s.running, len(s.queue)
}

// queueTask is the task label of a queue wait: one of Tasks, the gateway,
// or other.
func queueTask(ctx context.Context) string {
	if t, ok := ctx.Value(taskKey{}).(string); ok && t == gatewayTask {
		return t
	}
	return taskOf(ctx)
}
