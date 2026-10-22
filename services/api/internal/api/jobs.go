package api

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
	"github.com/parisnakitakejser/codesynapse/internal/telemetry"

	"github.com/parisnakitakejser/codesynapse/internal/ingest"
	"github.com/parisnakitakejser/codesynapse/internal/snapshot"
	"github.com/parisnakitakejser/codesynapse/internal/store"
	"github.com/parisnakitakejser/codesynapse/services/api/internal/aitasks"
)

// maxJobHistory bounds how many finished jobs are kept in memory.
const maxJobHistory = 50

// JobState is the lifecycle state of an ingest job.
type JobState string

// Job states.
const (
	JobRunning   JobState = "running"
	JobSucceeded JobState = "succeeded"
	JobFailed    JobState = "failed"
)

// Job is one ingest run.
type Job struct {
	ID         string   `json:"id"`
	Repo       string   `json:"repo"`
	OrgID      string   `json:"-"`
	Kind       string   `json:"kind"`
	Branch     string   `json:"branch,omitempty"`
	State      JobState `json:"state"`
	StartedAt  string   `json:"started_at"`
	FinishedAt string   `json:"finished_at,omitempty"`
	Nodes      int      `json:"nodes,omitempty"`
	Edges      int      `json:"edges,omitempty"`
	Error      string   `json:"error,omitempty"`
	// Task and Result are set for AI jobs: the task name and the review id.
	Task   string `json:"task,omitempty"`
	Result string `json:"result,omitempty"`
	// Waiting is true while the job waits for another job to finish; jobs
	// run one at a time.
	Waiting bool `json:"waiting,omitempty"`
}

// Jobs runs ingest jobs, at most one per repository at a time and one
// overall, since type-checking a large module is memory hungry.
type Jobs struct {
	ctx         context.Context //nolint:containedctx // lifetime of the job runner, not a request
	st          store.Writer
	logger      *slog.Logger
	commitLimit int
	network     bool
	vulnScan    bool
	// licenseLookup is set after NewJobs; it is read only by running jobs.
	licenseLookup bool
	sem           chan struct{}

	mu   sync.Mutex
	jobs map[string]*Job
}

// NewJobs returns a runner whose jobs are cancelled with ctx.
func NewJobs(ctx context.Context, st store.Writer, logger *slog.Logger, commitLimit int, allowNetwork, vulnScan bool) *Jobs {
	return &Jobs{ctx: ctx, st: st, logger: logger, commitLimit: commitLimit, network: allowNetwork, vulnScan: vulnScan, sem: make(chan struct{}, 1), jobs: map[string]*Job{}}
}

// JobOptions customise a job.
type JobOptions struct {
	OrgID string
	// Authorize checks current access after queueing and before graph storage.
	Authorize func(context.Context) error
	// Prepare runs before ingest (clone, fetch + checkout).
	Prepare func(ctx context.Context) error
	// Branch is recorded on the Repository node.
	Branch string
	// Kind labels the job in the UI: ingest, clone, sync or ai.
	Kind string
	// Task names the AI task for display.
	Task string
	// After runs in the background once an ingest succeeded (automation).
	After func(ctx context.Context)
	// Run replaces ingest with arbitrary work (AI tasks); its result (a
	// review id) is stored on the job.
	Run func(ctx context.Context) (string, error)
	// Done is called once the job ended, whatever the outcome.
	Done func(err error)
}

// Start queues an ingest of root into repo.
func (j *Jobs) Start(repo, root string, opts JobOptions) (Job, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, existing := range j.jobs {
		if existing.Repo == repo && existing.State == JobRunning {
			return Job{}, fmt.Errorf("an ingest of %q is already running (job %s)", repo, existing.ID)
		}
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return Job{}, err
	}
	kind := opts.Kind
	if kind == "" {
		kind = "ingest"
	}
	job := &Job{ID: hex.EncodeToString(b), Repo: repo, OrgID: opts.OrgID, Kind: kind, Branch: opts.Branch, State: JobRunning, StartedAt: time.Now().UTC().Format(time.RFC3339), Task: opts.Task, Waiting: true}
	j.jobs[job.ID] = job
	j.prune()
	go j.run(job, root, opts)
	return *job, nil
}

func (j *Jobs) run(job *Job, root string, opts JobOptions) {
	fin := func(job *Job, nodes, edges int, err error) {
		j.finish(job, nodes, edges, err)
		if opts.Done != nil {
			opts.Done(err)
		}
	}
	select {
	case j.sem <- struct{}{}:
		defer func() { <-j.sem }()
		j.mu.Lock()
		job.Waiting = false
		j.mu.Unlock()
	case <-j.ctx.Done():
		fin(job, 0, 0, j.ctx.Err())
		return
	}
	ctx := logx.WithLogger(j.ctx, j.logger.With("job", job.ID, "repo", job.Repo))
	ctx = telemetry.WithTenant(ctx, opts.OrgID)
	if err := checkJob(ctx, opts.Authorize); err != nil {
		fin(job, 0, 0, err)
		return
	}
	if opts.Run != nil {
		result, err := opts.Run(ctx)
		if errors.Is(err, aitasks.ErrNothingToDo) {
			err = nil // nothing new to analyse is a normal outcome
		}
		j.mu.Lock()
		job.Result = result
		j.mu.Unlock()
		fin(job, 0, 0, err)
		return
	}
	if opts.Prepare != nil {
		if err := opts.Prepare(ctx); err != nil {
			fin(job, 0, 0, err)
			return
		}
	}
	started := time.Now()
	var previous string
	if reader, ok := j.st.(store.Reader); ok {
		var err error
		previous, err = ingest.IdentityBaseline(ctx, reader, job.Repo)
		if err != nil {
			fin(job, 0, 0, err)
			return
		}
	}
	g, err := ingest.Run(ctx, ingest.Options{Repo: job.Repo, Root: root, CommitLimit: j.commitLimit, AllowNetwork: j.network, VulnScan: j.vulnScan, LicenseLookup: j.licenseLookup, Branch: opts.Branch, PreviousIdentities: previous})
	if err == nil {
		err = j.st.EnsureSchema(ctx)
	}
	if err == nil {
		err = checkJob(ctx, opts.Authorize)
	}
	if err == nil {
		err = j.st.ReplaceRepo(ctx, job.Repo, g)
		if hs, ok := j.st.(snapshot.Store); ok && err == nil {
			snapshot.Record(ctx, hs, job.Repo)
		}
	}
	nodes, edges := 0, 0
	if g != nil {
		nodes, edges = len(g.Nodes()), len(g.Edges())
	}
	system, outcome := store.System(j.st), "ok"
	if err != nil {
		outcome = "error"
	} else {
		telemetry.IngestNodes.AddContext(ctx, float64(nodes), system)
	}
	telemetry.IngestRuns.IncContext(ctx, system, outcome)
	telemetry.IngestDuration.ObserveContext(ctx, time.Since(started).Seconds(), system)
	fin(job, nodes, edges, err)
	if err == nil && opts.After != nil {
		go func() {
			if checkJob(j.ctx, opts.Authorize) == nil {
				opts.After(j.ctx)
			}
		}()
	}
}

func (j *Jobs) finish(job *Job, nodes, edges int, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	job.Nodes, job.Edges = nodes, edges
	if err != nil {
		job.State = JobFailed
		job.Error = err.Error()
		j.logger.Error("ingest failed", "job", job.ID, "repo", job.Repo, "error", err)
		return
	}
	job.State = JobSucceeded
}

// prune drops the oldest finished jobs beyond maxJobHistory. Callers hold mu.
func (j *Jobs) prune() {
	if len(j.jobs) <= maxJobHistory {
		return
	}
	var finished []*Job
	for _, job := range j.jobs {
		if job.State != JobRunning {
			finished = append(finished, job)
		}
	}
	slices.SortFunc(finished, func(a, b *Job) int { return cmp.Compare(a.StartedAt, b.StartedAt) })
	// Queued and running jobs are never dropped, so there may be fewer finished
	// jobs than the excess.
	for _, job := range finished[:min(len(finished), max(0, len(j.jobs)-maxJobHistory))] {
		delete(j.jobs, job.ID)
	}
}

// Get returns a snapshot of one job.
func (j *Jobs) Get(id string) (Job, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *job, true
}

// List returns snapshots of all jobs, newest first.
func (j *Jobs) List() []Job {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Job, 0, len(j.jobs))
	for _, job := range j.jobs {
		out = append(out, *job)
	}
	slices.SortFunc(out, func(a, b Job) int { return cmp.Or(cmp.Compare(b.StartedAt, a.StartedAt), cmp.Compare(a.ID, b.ID)) })
	return out
}
