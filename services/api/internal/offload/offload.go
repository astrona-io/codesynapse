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

	"github.com/parisnakitakejser/codesynapse/internal/logx"
	"github.com/parisnakitakejser/codesynapse/internal/memory"
	"github.com/parisnakitakejser/codesynapse/internal/memory/extract"
	"github.com/parisnakitakejser/codesynapse/internal/store"
	"github.com/parisnakitakejser/codesynapse/services/api/internal/llm"
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
	ID         string `json:"id"`
	Repo       string `json:"repo"`
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

	turns []extract.Turn
}

// Worker runs extraction jobs.
type Worker struct {
	st     Store
	llm    *llm.Registry
	logger *slog.Logger
	queue  chan string

	// OnSaved runs after a job saved facts (to embed new entities soon).
	OnSaved func()

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

// Submit validates and queues an extraction without waiting for it.
func (w *Worker) Submit(repo, source string, turns []extract.Turn) (Job, error) {
	if len(turns) == 0 || len(turns) > extract.MaxTurns {
		return Job{}, fmt.Errorf("give 1 to %d turns", extract.MaxTurns)
	}
	if _, err := w.Provider(); err != nil {
		return Job{}, err
	}
	id, err := newID()
	if err != nil {
		return Job{}, err
	}
	j := &Job{ID: id, Repo: repo, Source: cmp.Or(source, "agent"), State: Queued, Turns: len(turns),
		QueuedAt: w.now().UTC().Format(time.RFC3339), turns: slices.Clone(turns)}
	w.mu.Lock()
	defer w.mu.Unlock()
	select {
	case w.queue <- id:
	default:
		return Job{}, ErrBusy
	}
	w.jobs[id] = j
	w.order = append(w.order, id)
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
	w.mu.Lock()
	j := w.jobs[id]
	if j == nil {
		w.mu.Unlock()
		return
	}
	j.State, j.StartedAt = Running, w.now().UTC().Format(time.RFC3339)
	turns := j.turns
	w.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, jobTimeout)
	defer cancel()
	res, err := w.extract(ctx, j.Repo, j.Source, turns, func(provider, model string) {
		w.mu.Lock()
		j.Provider, j.Model = provider, model
		w.mu.Unlock()
	})

	w.mu.Lock()
	defer w.mu.Unlock()
	j.turns = nil
	j.FinishedAt = w.now().UTC().Format(time.RFC3339)
	j.Extracted, j.Saved, j.Dropped = res.extracted, res.saved, res.dropped
	if err != nil {
		j.State, j.Error = Failed, err.Error()
		w.logger.Warn("memory extraction failed", "job", j.ID, "repo", j.Repo, "provider", j.Provider, logx.Error(err))
		return
	}
	j.State = Succeeded
	if j.Saved > 0 && w.OnSaved != nil {
		w.OnSaved()
	}
	w.logger.Info("memory extraction done", "job", j.ID, "repo", j.Repo, "saved", j.Saved, "dropped", j.Dropped)
}

type outcome struct{ extracted, saved, dropped int }

func (w *Worker) extract(ctx context.Context, repo, source string, turns []extract.Turn, started func(provider, model string)) (outcome, error) {
	provider, err := w.Provider()
	if err != nil {
		return outcome{}, err
	}
	g, err := w.st.SearchMemory(ctx, repo, "", extract.MaxKnown, false)
	if err != nil {
		return outcome{}, fmt.Errorf("reading known entities: %w", err)
	}
	known := make([]string, 0, len(g.Entities))
	for _, e := range g.Entities {
		known = append(known, e.Name)
	}
	sys, user, err := extract.Prompt(known, turns)
	if err != nil {
		return outcome{}, err
	}
	cfg, _ := w.llm.Config(provider)
	started(provider, cfg.Model)
	resp, err := w.llm.Complete(ctx, provider, llm.Request{System: sys, User: user, MaxTokens: maxAnswer})
	if err != nil {
		return outcome{}, fmt.Errorf("model %s: %w", provider, err)
	}
	res, err := extract.Parse(resp.Text, extract.DefaultMinConfidence)
	if err != nil {
		return outcome{dropped: res.Dropped}, err
	}
	out := outcome{extracted: len(res.Triples) + res.Dropped, dropped: res.Dropped}
	if len(res.Triples) == 0 {
		return out, nil
	}
	label := fmt.Sprintf("%s (extracted by %s)", source, cmp.Or(resp.Model, cfg.Model, provider))
	b, err := memory.Prepare(repo, res.Triples, label, w.now())
	if err != nil {
		return out, fmt.Errorf("preparing triples: %w", err)
	}
	if err := w.st.UpsertMemory(ctx, b); err != nil {
		return out, fmt.Errorf("saving triples: %w", err)
	}
	out.saved = len(b.Facts) + len(b.Aliases)
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
	c.turns = nil
	return c
}

func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
