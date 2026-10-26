// Package offload extracts memory triples from conversation turns in the
// background, with a local model only, and saves them to agent memory.
//
// Submit never blocks: it validates, queues and returns a job. One worker
// runs jobs in order; the turns are held in memory until the job ran and
// are never stored.
package offload

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/logx"
	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/memory/extract"
	"github.com/astrona-io/codesynapse/internal/redact"
	"github.com/astrona-io/codesynapse/internal/sessionmemory"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

// Task is the routing task for extraction (AI models page).
const Task = "memory"

const (
	queueSize  = 32
	keepJobs   = 200
	jobTimeout = 3 * time.Minute
	maxAnswer  = 4000 // output tokens for the triples JSON
)

// Job states.
const (
	Queued    = "queued"
	Running   = "running"
	Succeeded = "succeeded"
	Failed    = "failed"
)

var (
	// ErrNoLocalModel means extraction would need a cloud model, which this
	// feature never uses: conversations often contain code.
	ErrNoLocalModel = errors.New("memory extraction runs only on a local model; route the \"memory\" task to a local provider on the AI models page (agents can still save triples themselves)")
	// ErrBusy means the queue is full.
	ErrBusy = errors.New("memory extraction queue is full; try again shortly")
)

// Store is what the worker reads and writes.
type Store interface {
	store.MemoryStore
}

// Job is one extraction; turns are dropped when it finishes.
type Job struct {
	Execution     execution.Identity `json:"execution"`
	SessionMemory bool               `json:"session_memory"`
	expiresAt     time.Time
	ID            string `json:"id"`
	Repo          string `json:"repo"`
	// Namespace is where the facts go: the repository, or "org|repo" when an
	// organisation asked (organisations sharing a graph keep separate memory).
	Namespace  string `json:"-"`
	Source     string `json:"source"`
	State      string `json:"state"`
	Turns      int    `json:"turns"`
	Extracted  int    `json:"extracted"`
	Saved      int    `json:"saved"`
	Dropped    int    `json:"dropped"`
	Provider   string `json:"provider,omitempty"`
	Model      string `json:"model,omitempty"`
	Error      string `json:"error,omitempty"`
	QueuedAt   string `json:"queued_at"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`

	turns     []extract.Turn
	authorize func(context.Context) error
	tool      string
}

// Worker runs extraction jobs.
type Worker struct {
	st     Store
	llm    *llm.Registry
	logger *slog.Logger
	queue  chan string

	// OnSaved runs after a job saved facts (to embed new entities soon).
	OnSaved        func()
	OnSessionSaved func(context.Context, sessionmemory.Scope, func(context.Context) error)
	// OnFinished hears about every job that succeeded, with the facts it
	// saved (for the statistics); it runs on its own goroutine.
	OnFinished func(ctx context.Context, org, repo string, saved int)

	mu    sync.Mutex
	jobs  map[string]*Job
	order []string
	now   func() time.Time
}

// New returns a worker; call Run to start it.
func New(st Store, reg *llm.Registry, logger *slog.Logger) *Worker {
	return &Worker{st: st, llm: reg, logger: logger, queue: make(chan string, queueSize), jobs: map[string]*Job{}, now: time.Now}
}

// Provider returns the local provider routed to extraction, or ErrNoLocalModel.
func (w *Worker) Provider() (string, error) {
	if w.llm == nil {
		return "", ErrNoLocalModel
	}
	name, err := w.llm.ProviderFor(Task, "")
	if err != nil {
		return "", ErrNoLocalModel
	}
	if cfg, ok := w.llm.Config(name); !ok || !cfg.Local() {
		return "", ErrNoLocalModel
	}
	return name, nil
}

// Submit validates and queues an extraction into namespace without waiting
// for it ("" means the repository's own namespace).
func (w *Worker) Submit(repo, namespace, source string, turns []extract.Turn) (Job, error) {
	return w.SubmitAuthorized(repo, namespace, source, turns, nil)
}

// SubmitAuthorized rechecks the initiating caller before extraction and storage.
func (w *Worker) SubmitAuthorized(repo, namespace, source string, turns []extract.Turn, authorize func(context.Context) error) (Job, error) {
	return w.SubmitAuthorizedContext(context.Background(), repo, namespace, source, turns, authorize)
}

// SubmitAuthorizedContext carries accepted tool attribution into queued work.
func (w *Worker) SubmitAuthorizedContext(ctx context.Context, repo, namespace, source string, turns []extract.Turn, authorize func(context.Context) error) (Job, error) {
	return w.submit(ctx, repo, namespace, source, turns, authorize, false, time.Time{})
}

// SubmitSession queues transient turns for the current session's private memory.
func (w *Worker) SubmitSession(ctx context.Context, repo, namespace, source string, turns []extract.Turn, expiresAt time.Time, authorize func(context.Context) error) (Job, error) {
	return w.submit(ctx, repo, namespace, source, turns, authorize, true, expiresAt)
}

func (w *Worker) submit(ctx context.Context, repo, namespace, source string, turns []extract.Turn, authorize func(context.Context) error, session bool, expiresAt time.Time) (Job, error) {
	if err := ctx.Err(); err != nil {
		return Job{}, err
	}
	identity, err := execution.Require(ctx)
	if err != nil {
		return Job{}, err
	}
	if authorize == nil {
		return Job{}, errors.New("execution requires a session and permission recheck")
	}

	if len(turns) == 0 || len(turns) > extract.MaxTurns {
		return Job{}, fmt.Errorf("give 1 to %d turns", extract.MaxTurns)
	}
	turns = slices.Clone(turns)
	total := 0
	for i := range turns {
		total += len(turns[i].Text)
		if len(turns[i].Text) > 4*extract.MaxTurnChars || total > 4*extract.MaxInputChars {
			return Job{}, errors.New("conversation turns exceed the extraction limit")
		}
		turns[i].Text = redact.Free(turns[i].Text)
	}
	if _, err := w.Provider(); err != nil {
		return Job{}, err
	}
	id, err := newID()
	if err != nil {
		return Job{}, err
	}
	source = redact.Free(source)
	if runes := []rune(source); len(runes) > 120 {
		source = string(runes[:120])
	}
	j := &Job{Execution: identity, SessionMemory: session, expiresAt: expiresAt, ID: id, Repo: repo, Namespace: cmp.Or(namespace, repo), Source: cmp.Or(source, "agent"), State: Queued, Turns: len(turns),
		QueuedAt: w.now().UTC().Format(time.RFC3339), turns: slices.Clone(turns), authorize: authorize, tool: telemetry.Tool(ctx)}
	w.mu.Lock()
	defer w.mu.Unlock()
	select {
	case w.queue <- id:
	default:
		return Job{}, ErrBusy
	}
	w.jobs[id] = j
	w.order = append(w.order, id)
	ctx = telemetry.WithTenant(ctx, memory.OrgOf(j.Namespace))
	tenant := telemetry.Tenant(ctx)
	telemetry.OffloadTurns.AddContext(ctx, float64(len(turns)), tenant)
	chars := 0
	for _, t := range turns {
		chars += len(t.Text)
	}
	telemetry.OffloadTokens.AddContext(ctx, float64(chars/4), tenant)
	telemetry.RecordStep(ctx, telemetry.StepOffloadQueue, true)
	telemetry.RecordStepTokens(ctx, telemetry.StepOffloadQueue, telemetry.KindContextOut, telemetry.EstimateTokens(chars))
	for len(w.order) > keepJobs {
		if old := w.jobs[w.order[0]]; old != nil && (old.State == Queued || old.State == Running) {
			break // never forget a job that has not finished
		}
		delete(w.jobs, w.order[0])
		w.order = w.order[1:]
	}
	return w.snapshot(j), nil
}

// Run processes jobs until ctx ends.
func (w *Worker) Run(ctx context.Context) {
	defer w.cancelPending()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-w.queue:
			w.run(ctx, id)
		}
	}
}

func (w *Worker) run(ctx context.Context, id string) {
	if ctx.Err() != nil {
		return
	}
	w.mu.Lock()
	j := w.jobs[id]
	if j == nil || j.State != Queued {
		w.mu.Unlock()
		return
	}
	j.State, j.StartedAt = Running, w.now().UTC().Format(time.RFC3339)
	turns := j.turns
	w.mu.Unlock()

	ctx, identityErr := execution.With(ctx, j.Execution)
	if identityErr != nil {
		w.mu.Lock()
		j.State, j.Error, j.turns = Failed, identityErr.Error(), nil
		w.mu.Unlock()
		return
	}
	ctx = telemetry.WithTool(ctx, j.tool)
	ctx, cancel := context.WithTimeout(llm.WithTask(telemetry.WithTenant(ctx, memory.OrgOf(j.Namespace)), "memory"), jobTimeout)
	defer cancel()
	res, err := w.extract(ctx, j.Namespace, j.Source, turns, j.authorize, j.SessionMemory, j.expiresAt, func(provider, model string) {
		w.mu.Lock()
		j.Provider, j.Model = provider, model
		w.mu.Unlock()
	})

	if err == nil && j.SessionMemory && w.OnSessionSaved != nil {
		w.OnSessionSaved(ctx, sessionmemory.Scope{TenantID: j.Execution.TenantID, SessionID: j.Execution.SessionID, Namespace: j.Namespace}, j.authorize)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	j.turns, j.authorize = nil, nil
	j.FinishedAt = w.now().UTC().Format(time.RFC3339)
	j.Extracted, j.Saved, j.Dropped = res.extracted, res.saved, res.dropped
	if err != nil {
		j.State, j.Error = Failed, err.Error()
		w.logger.Warn("memory extraction failed", "job", j.ID, "repo", j.Repo, "provider", j.Provider, logx.Error(err))
		return
	}
	j.State = Succeeded
	telemetry.MemoryFacts.AddContext(telemetry.WithModel(ctx, j.Provider, j.Model), float64(j.Saved), telemetry.Tenant(ctx), "offload")
	if j.Saved > 0 && w.OnSaved != nil {
		w.OnSaved()
	}
	if w.OnFinished != nil {
		go w.OnFinished(context.WithoutCancel(ctx), j.Execution.TenantID, j.Repo, j.Saved)
	}
	w.logger.Info("memory extraction done", "job", j.ID, "repo", j.Repo, "saved", j.Saved, "dropped", j.Dropped)
}

type outcome struct{ extracted, saved, dropped int }

func (w *Worker) extract(ctx context.Context, repo, source string, turns []extract.Turn, authorize func(context.Context) error, session bool, expiresAt time.Time, started func(provider, model string)) (outcome, error) {
	if authorize != nil {
		if err := authorize(ctx); err != nil {
			return outcome{}, err
		}
	}
	provider, err := w.Provider()
	if err != nil {
		return outcome{}, err
	}
	cfg, _ := w.llm.Config(provider)
	ctx = telemetry.WithModel(ctx, provider, cfg.Model)
	var g memory.Graph
	var sessionStore store.SessionMemoryStore
	var scope sessionmemory.Scope
	if session {
		id, identityErr := execution.Require(ctx)
		if identityErr != nil {
			return outcome{}, identityErr
		}
		scope = sessionmemory.Scope{TenantID: id.TenantID, SessionID: id.SessionID, Namespace: repo}
		var ok bool
		sessionStore, ok = w.st.(store.SessionMemoryStore)
		if !ok {
			return outcome{}, errors.New("session memory is unavailable")
		}
		g, err = sessionStore.ReadSessionMemory(ctx, scope, "", extract.MaxKnown)
	} else {
		g, err = w.st.SearchMemory(ctx, repo, "", extract.MaxKnown, false)
	}
	if err != nil {
		return outcome{}, fmt.Errorf("reading known entities: %w", err)
	}
	known := make([]string, 0, len(g.Entities))
	for _, e := range g.Entities {
		known = append(known, e.Name)
	}
	started(provider, cfg.Model)
	var total outcome
	for len(turns) > 0 {
		// Packed against the entities known now: earlier batches add names to
		// the prompt, so a plan made up front can outgrow the budget.
		var batch []extract.Turn
		batch, turns, err = w.nextBatch(ctx, provider, known, turns)
		if err != nil {
			return total, err
		}
		out, err := w.extractBatch(ctx, provider, cfg.Model, repo, source, known, batch, authorize, session, expiresAt, sessionStore, scope)
		total.extracted += out.extracted
		total.saved += out.saved
		total.dropped += out.dropped
		if err != nil {
			return total, err
		}
		for _, t := range out.names {
			if len(known) >= extract.MaxKnown {
				break
			}
			known = append(known, t)
		}
	}
	return total, nil
}

// nextBatch takes turns from the front, in order, while the prompt fits the
// provider's input budget, and returns them with the rest. A model call sees
// one batch, so a long chat becomes several calls instead of one the provider
// refuses. A single turn that cannot fit on its own is shortened until it does.
func (w *Worker) nextBatch(ctx context.Context, provider string, known []string, turns []extract.Turn) (batch, rest []extract.Turn, err error) {
	fits := func(batch []extract.Turn) (bool, error) {
		sys, user, err := extract.Prompt(known, batch)
		if err != nil {
			return false, err
		}
		_, err = w.llm.Preflight(ctx, provider, llm.Request{System: sys, User: user, MaxTokens: maxAnswer})
		if errors.Is(err, llm.ErrInputBudget) {
			return false, nil
		}
		return err == nil, err
	}
	for len(turns) > 0 {
		t := turns[0]
		for {
			ok, err := fits(append(batch[:len(batch):len(batch)], t))
			if err != nil {
				return nil, nil, err
			}
			if ok {
				break
			}
			if len(batch) > 0 {
				return batch, turns, nil
			}
			r := []rune(t.Text)
			if len(r) <= 200 {
				return nil, nil, fmt.Errorf("prompt exceeds input budget: %w", llm.ErrInputBudget)
			}
			t.Text = string(r[:len(r)/2]) + " …"
		}
		batch = append(batch, t)
		turns = turns[1:]
	}
	return batch, nil, nil
}

type batchOutcome struct {
	outcome
	names []string // entity names the batch saved, for the next batch to reuse
}

func (w *Worker) extractBatch(ctx context.Context, provider, model, repo, source string, known []string, turns []extract.Turn, authorize func(context.Context) error, session bool, expiresAt time.Time, sessionStore store.SessionMemoryStore, scope sessionmemory.Scope) (batchOutcome, error) {
	sys, user, err := extract.Prompt(known, turns)
	if err != nil {
		return batchOutcome{}, err
	}
	resp, err := w.llm.Complete(ctx, provider, llm.Request{System: sys, User: user, MaxTokens: maxAnswer})
	telemetry.RecordStep(ctx, telemetry.StepExtract, err == nil)
	if err != nil {
		return batchOutcome{}, fmt.Errorf("model %s: %w", provider, err)
	}
	// Reported usage when the provider gave it, else the size estimate.
	telemetry.RecordStepTokens(ctx, telemetry.StepExtract, telemetry.KindModelIn, cmp.Or(resp.InputTokens, telemetry.EstimateTokens(len(sys)+len(user))))
	telemetry.RecordStepTokens(ctx, telemetry.StepExtract, telemetry.KindModelOut, cmp.Or(resp.OutputTokens, telemetry.EstimateTokens(len(resp.Text))))
	res, err := extract.Parse(resp.Text, extract.DefaultMinConfidence)
	if err != nil {
		return batchOutcome{outcome: outcome{dropped: res.Dropped}}, err
	}
	out := batchOutcome{outcome: outcome{extracted: len(res.Triples) + res.Dropped, dropped: res.Dropped}}
	if len(res.Triples) == 0 {
		return out, nil
	}
	label := fmt.Sprintf("%s (extracted by %s)", source, cmp.Or(resp.Model, model, provider))
	b, err := memory.Prepare(repo, res.Triples, label, w.now())
	if err != nil {
		return out, fmt.Errorf("preparing triples: %w", err)
	}
	if authorize != nil {
		if err := authorize(ctx); err != nil {
			return out, err
		}
	}
	if session {
		err = sessionStore.UpsertSessionMemory(ctx, scope, res.Triples, label, expiresAt)
	} else {
		err = w.st.UpsertMemory(ctx, b)
	}
	if err != nil {
		return out, fmt.Errorf("saving triples: %w", err)
	}
	out.saved = len(b.Facts) + len(b.Aliases)
	for _, t := range res.Triples {
		out.names = append(out.names, t.Subject, t.Object)
	}
	return out, nil
}

// Get returns a job by ID.
func (w *Worker) Get(id string) (Job, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	j, ok := w.jobs[id]
	if !ok {
		return Job{}, false
	}
	return w.snapshot(j), true
}

// List returns recent jobs, newest first, optionally for one repository.
func (w *Worker) List(repo string) []Job {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := []Job{}
	for i := len(w.order) - 1; i >= 0; i-- {
		if j := w.jobs[w.order[i]]; j != nil && (repo == "" || j.Repo == repo) {
			out = append(out, w.snapshot(j))
		}
	}
	return out
}

func (w *Worker) snapshot(j *Job) Job {
	c := *j
	c.turns, c.authorize = nil, nil
	return c
}

func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Shutdown drops transient turns even when they never reached the worker.
func (w *Worker) cancelPending() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, j := range w.jobs {
		if j.State == Queued || j.State == Running {
			j.State, j.Error, j.FinishedAt = Failed, "worker stopped before completion", w.now().UTC().Format(time.RFC3339)
			j.turns, j.authorize = nil, nil
		}
	}
}
