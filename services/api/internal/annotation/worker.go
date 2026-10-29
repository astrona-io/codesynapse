package annotation

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/logx"
	"github.com/astrona-io/codesynapse/internal/prose"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

// Persist must atomically check the active revision/policy and save the entire
// result. Repeated Job.ID deliveries must not create duplicate records.
type Persist func(context.Context, Result) error

// Current verifies pinned symbol membership, revision and trusted policy.
type Current func(context.Context, Batch) error

type pending struct {
	job       Job
	batch     Batch
	authorize func(context.Context) error
	current   Current
	tool      string
	cancel    context.CancelFunc
}

// Worker owns a bounded queue. Run has one lifetime; completed status is retained
// in bounded memory, while persistence is supplied by the tenant control plane.
type Worker struct {
	registry *llm.Registry
	persist  Persist
	queue    chan string
	mu       sync.Mutex
	jobs     map[string]*pending
	order    []string
	started  bool
	closed   bool
}

// New returns a stopped worker ready to accept work before Run starts.
func New(registry *llm.Registry, persist Persist) *Worker {
	return &Worker{registry: registry, persist: persist, queue: make(chan string, MaxPending), jobs: make(map[string]*pending)}
}

func (w *Worker) provider() (string, llm.ProviderConfig, error) {
	if w.registry == nil {
		return "", llm.ProviderConfig{}, ErrLocal
	}
	name, err := w.registry.ProviderFor(Task, "")
	if err != nil {
		return "", llm.ProviderConfig{}, ErrLocal
	}
	cfg, ok := w.registry.Config(name)
	if !ok || !cfg.Local() {
		return "", llm.ProviderConfig{}, ErrLocal
	}
	return name, cfg, nil
}

// Submit checks identity and policy before queueing. The HTTP request may end
// after acceptance; the worker restores its identity into the service context.
func (w *Worker) Submit(ctx context.Context, in Batch, authorize func(context.Context) error, current Current) (Job, error) {
	identity, err := execution.Require(ctx)
	if err != nil {
		return Job{}, err
	}
	if authorize == nil || current == nil || w.persist == nil {
		return Job{}, ErrAuthorization
	}
	batch, err := prepare(in)
	if err != nil {
		return Job{}, err
	}
	if err := authorize(ctx); err != nil {
		return Job{}, err
	}
	if err := current(ctx, batch); err != nil {
		return Job{}, err
	}
	if _, _, err := w.provider(); err != nil {
		return Job{}, err
	}
	j := Job{ID: jobID(identity, batch), Execution: identity, Graph: batch.Graph, Commit: batch.Commit, Revision: batch.Revision, PolicyDigest: batch.PolicyDigest, State: Queued, QueuedAt: time.Now().UTC()}
	for _, symbol := range batch.Symbols {
		j.SymbolIDs = append(j.SymbolIDs, symbol.ID)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return Job{}, ErrClosed
	}
	if old := w.jobs[j.ID]; old != nil {
		return snapshot(old.job), nil
	}
	active, tenantActive := 0, 0
	for _, old := range w.jobs {
		if old.job.State == Queued || old.job.State == Running {
			active++
			if old.job.Execution.TenantID == identity.TenantID {
				tenantActive++
			}
		}
	}
	if active >= MaxPending || tenantActive >= MaxTenantPending {
		return Job{}, ErrBusy
	}
	select {
	case w.queue <- j.ID:
	default:
		return Job{}, ErrBusy
	}
	w.jobs[j.ID] = &pending{job: j, batch: batch, authorize: authorize, current: current, tool: telemetry.Tool(ctx)}
	w.order = append(w.order, j.ID)
	w.prune()
	return snapshot(j), nil
}

func (w *Worker) prune() {
	for i := 0; len(w.order) > MaxHistory && i < len(w.order); {
		id := w.order[i]
		if j := w.jobs[id]; j.job.State == Queued || j.job.State == Running {
			i++
			continue
		}
		delete(w.jobs, id)
		w.order = slices.Delete(w.order, i, i+1)
	}
}

// Get scopes job metadata to the original runtime identity.
func (w *Worker) Get(ctx context.Context, id string) (Job, bool) {
	identity, err := execution.Require(ctx)
	if err != nil {
		return Job{}, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	p := w.jobs[id]
	if p == nil || p.job.Execution != identity {
		return Job{}, false
	}
	return snapshot(p.job), true
}

// Cancel stops queued or running work owned by this runtime.
func (w *Worker) Cancel(ctx context.Context, id string) bool {
	identity, err := execution.Require(ctx)
	if err != nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	p := w.jobs[id]
	if p == nil || p.job.Execution != identity {
		return false
	}
	if p.job.State == Queued {
		finish(ctx, p, Cancelled, "annotation cancelled")
	} else if p.job.State == Running && p.cancel != nil {
		p.cancel()
	}
	return true
}

// Run starts a fixed number of consumers and waits for cancellation. Calling it
// again cannot replay abandoned work or restart a stopped worker.
func (w *Worker) Run(ctx context.Context) {
	w.mu.Lock()
	if w.started || w.closed {
		w.mu.Unlock()
		return
	}
	w.started = true
	w.mu.Unlock()
	var group sync.WaitGroup
	for range Concurrency {
		group.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case id := <-w.queue:
					w.run(ctx, id)
				}
			}
		})
	}
	<-ctx.Done()
	group.Wait()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	for _, p := range w.jobs {
		if p.job.State == Queued || p.job.State == Running {
			finish(ctx, p, Cancelled, "annotation worker stopped")
		}
	}
}

func (w *Worker) run(ctx context.Context, id string) {
	if ctx.Err() != nil {
		return
	}
	w.mu.Lock()
	p := w.jobs[id]
	if p == nil || p.job.State != Queued {
		w.mu.Unlock()
		return
	}
	workCtx, err := execution.With(ctx, p.job.Execution)
	if err != nil {
		finish(ctx, p, Failed, "annotation context is unavailable")
		w.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(workCtx, JobTimeout)
	p.cancel = cancel
	p.job.State, p.job.StartedAt = Running, time.Now().UTC()
	j, batch, authorize, current, tool := snapshot(p.job), p.batch, p.authorize, p.current, p.tool
	w.mu.Unlock()
	defer cancel()
	ctx = llm.WithTask(telemetry.WithTool(telemetry.WithTenant(ctx, j.Execution.TenantID), tool), Task)
	result, err := w.evaluate(ctx, j, batch, authorize, current)
	w.mu.Lock()
	defer w.mu.Unlock()
	p.job.Provider, p.job.Model = result.Job.Provider, result.Job.Model
	if err != nil {
		state, message := Failed, "annotation failed; verify the local model, access and active policy"
		if errors.Is(err, context.Canceled) {
			state, message = Cancelled, "annotation cancelled"
		}
		finish(ctx, p, state, message)
		return
	}
	finish(ctx, p, Succeeded, "")
}

func (w *Worker) evaluate(ctx context.Context, j Job, batch Batch, authorize func(context.Context) error, current Current) (Result, error) {
	result := Result{Job: j, Fingerprints: make(map[string]string, len(batch.Symbols))}
	for _, s := range batch.Symbols {
		result.Fingerprints[s.ID] = s.Fingerprint
	}
	check := func() error {
		if _, err := execution.Require(ctx); err != nil {
			return err
		}
		if err := authorize(ctx); err != nil {
			return err
		}
		return current(ctx, batch)
	}
	if err := check(); err != nil {
		return result, err
	}
	name, cfg, err := w.provider()
	if err != nil {
		return result, err
	}
	result.Job.Provider, result.Job.Model = name, cfg.Model
	input, err := json.Marshal(struct {
		Symbols       []Symbol `json:"symbols"`
		TaxonomyHints []string `json:"taxonomy_hints"`
	}{batch.Symbols, batch.TaxonomyHints})
	// One token per byte is a conservative bound without claiming a tokenizer
	// estimate. Reserve room for chat framing and the output budget.
	outputTokens := min(8192, cfg.MaxOutputTokens)
	if err != nil || cfg.MaxInputTokens < len(input)+len(system)+256 || outputTokens < 1 {
		return result, ErrInvalid
	}
	answer, err := w.registry.Complete(ctx, name, llm.Request{System: system, User: string(input), MaxTokens: outputTokens})
	if err != nil {
		return result, err
	}
	result.Items, err = parse(answer.Text, batch)
	if err != nil {
		return result, err
	}
	lintSummaries(ctx, result.Items)
	if err := check(); err != nil {
		return result, err
	}
	result.Job.State, result.Job.FinishedAt = Succeeded, time.Now().UTC()
	if err := w.persist(store.WithCommitCheck(ctx, authorize), result); err != nil {
		return result, err
	}
	return result, nil
}

// lintSummaries logs how many greetings and filler phrases the summaries had.
// It measures only: summaries are kept as they are, and no text is logged.
func lintSummaries(ctx context.Context, items []Item) {
	var filler, greeting int
	for _, it := range items {
		f, g := prose.Counts(it.Summary)
		filler, greeting = filler+f, greeting+g
	}
	logx.FromContext(ctx).Info("ai task prose lint", "task", Task, "filler", filler, "greetings", greeting)
}

func finish(ctx context.Context, p *pending, state, message string) {
	p.job.State, p.job.Error, p.job.FinishedAt = state, message, time.Now().UTC()
	ctx = telemetry.WithTool(telemetry.WithTenant(ctx, p.job.Execution.TenantID), p.tool)
	if p.job.Provider != "" {
		ctx = telemetry.WithModel(ctx, p.job.Provider, p.job.Model)
	}
	telemetry.AnnotationJobs.AddContext(ctx, 1, state)
	telemetry.AnnotationDuration.ObserveContext(ctx, time.Since(p.job.QueuedAt).Seconds(), state)
	p.batch, p.authorize, p.current, p.cancel = Batch{}, nil, nil, nil
}

func snapshot(j Job) Job {
	j.SymbolIDs = slices.Clone(j.SymbolIDs)
	return j
}
