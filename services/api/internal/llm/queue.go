package llm

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// gatewayTask labels the queue waits of agent gateway traffic, which runs
// under no task of its own.
const gatewayTask = "gateway"

// slots queues requests for one provider: at most cap run, the others wait.
// A nil *slots (provider added after start, or a cloud provider without
// max_concurrent) never limits and records nothing.
type slots struct {
	ch            chan struct{}
	waiting       atomic.Int32
	provider      string
	model         string
	capacity      sync.Once
	mu            sync.Mutex
	tenantRunning map[string]int
	tenantWaiting map[string]int
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
	return &slots{ch: make(chan struct{}, n), provider: p.Name, model: p.Model,
		tenantRunning: map[string]int{}, tenantWaiting: map[string]int{}}
}

// acquire waits for a free slot or the end of ctx and returns the release.
// Every wait is recorded, also one that found a slot at once or ended
// before one came free, so the statistics count every request.
func (s *slots) acquire(ctx context.Context) (func(), error) {
	if s == nil {
		return func() {}, nil
	}
	mctx := telemetry.WithTool(telemetry.WithModel(ctx, s.provider, s.model), "none")
	// Published with the first request, under its context: building the
	// registry has none. An idle process reports no capacity yet.
	s.capacity.Do(func() { telemetry.SetQueueCapacity(mctx, s.provider, cap(s.ch)) })
	task := queueTask(ctx)
	start := time.Now()
	select {
	case s.ch <- struct{}{}:
		s.load(mctx, 1, 0)
		telemetry.RecordQueueWait(mctx, task, telemetry.QueueAcquired, 0)
		return s.release(mctx), nil
	default:
	}
	s.waiting.Add(1)
	s.load(mctx, 0, 1)
	defer s.waiting.Add(-1)
	select {
	case s.ch <- struct{}{}:
		s.load(mctx, 1, -1)
		telemetry.RecordQueueWait(mctx, task, telemetry.QueueAcquired, time.Since(start))
		return s.release(mctx), nil
	case <-ctx.Done():
		s.load(mctx, 0, -1)
		outcome := telemetry.QueueCancelled
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			outcome = telemetry.QueueTimeout
		}
		telemetry.RecordQueueWait(mctx, task, outcome, time.Since(start))
		return nil, ctx.Err()
	}
}

func (s *slots) release(mctx context.Context) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			<-s.ch
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
	return len(s.ch), int(s.waiting.Load())
}

// queueTask is the task label of a queue wait: one of Tasks, the gateway,
// or other.
func queueTask(ctx context.Context) string {
	if t, ok := ctx.Value(taskKey{}).(string); ok && t == gatewayTask {
		return t
	}
	return taskOf(ctx)
}
