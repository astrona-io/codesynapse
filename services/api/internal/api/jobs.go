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

	"github.com/astrona-io/codesynapse/internal/accounts"
	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/logx"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/astrona-io/codesynapse/internal/ingest"
	"github.com/astrona-io/codesynapse/internal/ingestreport"
	"github.com/astrona-io/codesynapse/internal/snapshot"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/services/api/internal/aitasks"
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
	Identity   execution.Identity `json:"execution"`
	ID         string             `json:"id"`
	Repo       string             `json:"repo"`
	OrgID      string             `json:"-"`
	Kind       string             `json:"kind"`
	Branch     string             `json:"branch,omitempty"`
	State      JobState           `json:"state"`
	StartedAt  string             `json:"started_at"`
	FinishedAt string             `json:"finished_at,omitempty"`
	Nodes      int                `json:"nodes,omitempty"`
	Edges      int                `json:"edges,omitempty"`
	Error      string             `json:"error,omitempty"`
	// Task and Result are set for AI jobs: the task name and the review id.
	Task   string `json:"task,omitempty"`
	Result string `json:"result,omitempty"`
	// Waiting is true while the job waits for another job to finish; jobs
	// run one at a time.
	Waiting bool `json:"waiting,omitempty"`
	// Phase names the stage an index job is in; Step of Steps drives the
	// dashboard's progress bar. Zero Steps means no progress is known.
	Phase string `json:"phase,omitempty"`
	Step  int    `json:"step,omitempty"`
	Steps int    `json:"steps,omitempty"`
	// Attempts counts starts; above 1 the job was resumed after a restart.
	Attempts int `json:"attempts,omitempty"`
	durable  bool
	// leases is j.held when the job was queued (jobs_durable.go).
	leases context.Context
	// queuedAt starts the job's wait (codesynapse_job_wait_seconds).
	queuedAt time.Time
}

// Jobs runs ingest jobs, at most one per repository at a time and one
// overall, since type-checking a large module is memory hungry.
type Jobs struct {
	Report      func(context.Context, Job, ingestreport.Report) error
	Policy      func(context.Context, string, *repoconfig.Policy) (string, error)
	Publish     func(context.Context, string, func(context.Context) error) error
	Ingested    func(context.Context, string, *model.Graph, func(context.Context) error)
	ctx         context.Context //nolint:containedctx // lifetime of the job runner, not a request
	st          store.Writer
	logger      *slog.Logger
	commitLimit int
	network     bool
	vulnScan    bool
	// licenseLookup and dataflow are set after NewJobs; they are read only
	// by running jobs.
	licenseLookup bool
	dataflow      bool
	moduleProxy   bool
	sem           chan struct{}
	// store keeps jobs durable (jobs_durable.go); owner is this process;
	// kinds are the durable job kinds it runs (others are only recorded).
	store jobStore
	owner string
	kinds []string

	mu   sync.Mutex
	jobs map[string]*Job
	// held and drop: see leases (jobs_durable.go).
	held context.Context
	drop context.CancelCauseFunc
}

// UseStore makes every job durable in st and keeps this process's leases
// alive until the runner's context ends. kinds are the job kinds this
// process runs; jobs of other kinds are recorded for workers.
func (j *Jobs) UseStore(st jobStore, kinds []string) {
	j.store, j.owner, j.kinds = st, newOwner(), kinds
	j.held, j.drop = context.WithCancelCause(j.ctx)
	go j.heartbeat(j.ctx)
}

func (j *Jobs) runs(kind string) bool { return slices.Contains(j.kinds, kind) }

// idle reports whether no job of this process waits or runs.
func (j *Jobs) idle() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, job := range j.jobs {
		if job.State == JobRunning {
			return false
		}
	}
	return true
}

// NewJobs returns a runner whose jobs are cancelled with ctx.
func NewJobs(ctx context.Context, st store.Writer, logger *slog.Logger, commitLimit int, allowNetwork, vulnScan bool) *Jobs {
	return &Jobs{ctx: ctx, st: st, logger: logger, commitLimit: commitLimit, network: allowNetwork, vulnScan: vulnScan, sem: make(chan struct{}, 1), jobs: map[string]*Job{}}
}

// JobOptions customise a job.
type JobOptions struct {
	Identity execution.Identity
	OrgID    string
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
	// Durable describes the job for the store; required once UseStore ran.
	Durable *durableJob
}

// Start queues an ingest of root into repo.
func (j *Jobs) Start(repo, root string, opts JobOptions) (Job, error) {
	ctx, err := execution.With(j.ctx, opts.Identity)
	if err != nil {
		return Job{}, err
	}
	if opts.OrgID != "" && opts.Identity.TenantID != opts.OrgID {
		return Job{}, execution.ErrConflict
	}
	if err := checkJob(ctx, opts.Authorize); err != nil {
		return Job{}, err
	}
	if j.store != nil && opts.Durable == nil {
		return Job{}, errors.New("with accounts every job must be durable")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, existing := range j.jobs {
		if existing.Repo == repo && existing.State == JobRunning && (opts.Durable == nil || opts.Durable.resume == nil || existing.ID != opts.Durable.resume.ID) {
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
	now := time.Now().UTC()
	job := &Job{ID: hex.EncodeToString(b), Repo: repo, OrgID: opts.OrgID, Identity: opts.Identity, Kind: kind, Branch: opts.Branch, State: JobRunning, StartedAt: now.Format(time.RFC3339), Task: opts.Task, Waiting: true, queuedAt: now}
	runID := job.ID
	if d := opts.Durable; j.store != nil {
		job.durable = true
		if d.resume != nil {
			// Same job, next attempt; its ingest run gets its own ID so the
			// interrupted run is reconciled, not overwritten.
			job.ID, job.Attempts = d.resume.ID, d.resume.Attempts
			job.StartedAt = d.resume.CreatedAt.Format(time.RFC3339)
			if d.resume.Attempts == 0 {
				// A worker claiming a queued job: its wait began when the
				// job was recorded. A job started again after an interrupted
				// attempt reported that wait already; only this one is new.
				job.queuedAt = d.resume.CreatedAt
			}
			runID = fmt.Sprintf("%s.%d", job.ID, d.resume.Attempts+1)
		} else {
			rec := accounts.JobRecord{ID: job.ID, Kind: d.Kind, Repo: repo, OrgID: opts.OrgID, ActorID: d.ActorID, Via: d.Via, KeyID: d.KeyID,
				Role: d.Role, Execution: opts.Identity, Params: d.Params, Trace: d.Trace, Branch: opts.Branch, Task: opts.Task, State: accounts.JobQueued,
				LeaseOwner: j.owner, LeaseUntil: now.Add(jobLease), CreatedAt: now.Truncate(time.Microsecond)}
			local := j.runs(d.Kind)
			if !local {
				// A worker of this kind claims it (no owner, lease expired).
				rec.LeaseOwner, rec.LeaseUntil = "", time.Unix(0, 0).UTC()
			}
			addCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := j.store.AddJob(addCtx, rec)
			cancel()
			if errors.Is(err, accounts.ErrJobActive) {
				return Job{}, fmt.Errorf("%q already has a queued or running job", repo)
			}
			if err != nil {
				return Job{}, fmt.Errorf("recording the job: %w", err)
			}
			if !local {
				return fromRecord(rec), nil
			}
		}
	}
	// abandon ends a recorded job that never got going (mu is held).
	abandon := func(cause error) error {
		if job.durable {
			j.recordEnd(ctx, job.ID, accounts.JobOutcome{State: accounts.JobFailed, Error: cause.Error(), At: time.Now().UTC()})
		}
		return cause
	}
	var ledger *ingestreport.Ledger
	if opts.Run == nil {
		ledger, err = ingestreport.New(runID, now)
		if err != nil {
			return Job{}, abandon(err)
		}
		if j.Report != nil {
			reportCtx, reportCancel := context.WithTimeout(ctx, 10*time.Second)
			err := j.writeReport(reportCtx, *job, ledger.Snapshot(), opts.Authorize)
			reportCancel()
			if err != nil {
				return Job{}, abandon(err)
			}
		}
	}
	j.jobs[job.ID] = job
	j.prune()
	result := *job
	job.leases = j.held
	go j.run(job, root, opts, ledger)
	return result, nil
}

func (j *Jobs) run(job *Job, root string, opts JobOptions, ledger *ingestreport.Ledger) {
	ctx, cancelTimeout := context.WithTimeout(j.ctx, time.Hour)
	defer cancelTimeout()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if job.durable && job.leases != nil {
		stop := context.AfterFunc(job.leases, func() { cancel(errLeaseLost) })
		defer stop()
	}
	if opts.Durable != nil {
		ctx = telemetry.WithTraceParent(ctx, opts.Durable.Trace)
	}
	ctx, span := telemetry.Tracer().Start(ctx, "job "+job.Kind, trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(attribute.String("job.kind", job.Kind), attribute.String("job.id", job.ID), attribute.Int("job.attempt", job.Attempts+1)))
	defer span.End()
	phase := func(name string) func(error) {
		_, s := telemetry.Tracer().Start(ctx, "job."+name)
		began := time.Now()
		return func(err error) {
			if err != nil {
				s.SetStatus(codes.Error, "failed")
			}
			s.End()
			telemetry.RecordIngestPhase(ctx, name, time.Since(began), err == nil)
		}
	}
	// Job metrics carry the job's organisation, which Start checked against
	// its execution identity, and a kind from the fixed set.
	metricsCtx := telemetry.WithTenant(context.WithoutCancel(ctx), opts.OrgID)
	kind := job.Kind
	if opts.Durable != nil {
		kind = opts.Durable.Kind
	}
	var claimed time.Time
	handedOver := false
	ctx, identityErr := execution.With(ctx, job.Identity)
	graphAttempted, reportPublished := false, false
	fin := func(job *Job, nodes, edges int, err error) {
		if ledger != nil && j.Report != nil && !reportPublished && identityErr == nil {
			status, commit := ingestreport.Failed, ingestreport.CommitNone
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				status = ingestreport.Cancelled
			}
			if graphAttempted {
				commit = ingestreport.CommitUnknown
			}
			report := ledger.Finish(status, commit)
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			if reportErr := j.Report(cleanupCtx, *job, report); reportErr != nil {
				j.logger.Error("ingest accounting remains pending", "job", job.ID, "error", reportErr)
				err = errors.Join(err, reportErr)
			}
			cleanupCancel()
		}
		j.finish(job, nodes, edges, err)
		if err != nil {
			span.SetStatus(codes.Error, "job failed")
		}
		outcome := telemetry.JobOK
		switch {
		case job.durable && j.leftBehind(ctx, err):
			outcome = telemetry.JobLeftBehind
			j.logger.Info("job left for another process to resume", "job", job.ID, "cause", context.Cause(ctx))
		case job.durable:
			j.endDurable(ctx, job, err)
		}
		switch {
		case outcome != telemetry.JobOK || err == nil:
		case handedOver:
			outcome = telemetry.JobLeftBehind
		case errors.Is(err, context.Canceled):
			outcome = telemetry.JobCancelled
		default:
			outcome = telemetry.JobError
		}
		telemetry.RecordJobFinished(metricsCtx, kind, outcome)
		if !claimed.IsZero() {
			telemetry.RecordJobDuration(metricsCtx, kind, time.Since(claimed))
		}
		if opts.Done != nil {
			opts.Done(err)
		}
	}
	select {
	case j.sem <- struct{}{}:
		defer func() { <-j.sem }()
		if job.durable {
			startCtx, startCancel := context.WithTimeout(ctx, 10*time.Second)
			now := time.Now().UTC()
			err := j.store.StartJob(startCtx, job.ID, j.owner, now.Add(jobLease), now)
			startCancel()
			if err != nil {
				// Another process took it over (or the store is down): do
				// not run it twice.
				j.logger.Warn("job not started: lease lost", "job", job.ID, "error", err)
				job.durable, handedOver = false, true
				fin(job, 0, 0, fmt.Errorf("job handed to another process or not recorded: %w", err))
				return
			}
			j.mu.Lock()
			job.Attempts++
			j.mu.Unlock()
		}
		j.mu.Lock()
		job.Waiting = false
		j.mu.Unlock()
		claimed = time.Now()
		telemetry.RecordJobWait(metricsCtx, kind, claimed.Sub(job.queuedAt))
	case <-ctx.Done():
		fin(job, 0, 0, ctx.Err())
		return
	}
	if identityErr != nil {
		fin(job, 0, 0, identityErr)
		return
	}
	ctx = logx.WithLogger(ctx, j.logger.With("job", job.ID, "repo", job.Repo))
	ctx = telemetry.WithTenant(ctx, opts.OrgID)
	if ledger != nil {
		ctx = ingestreport.WithLedger(ctx, ledger)
		ledger.SetCheckpoint(func(checkCtx context.Context, report ingestreport.Report) error {
			if j.Report == nil {
				return nil
			}
			if err := checkJob(checkCtx, opts.Authorize); err != nil {
				return err
			}
			return j.writeReport(checkCtx, *job, report, opts.Authorize)
		})
	}
	if err := checkJob(ctx, opts.Authorize); err != nil {
		fin(job, 0, 0, err)
		return
	}
	if opts.Run != nil {
		result, err := opts.Run(ctx)
		if err == nil {
			err = checkJob(ctx, opts.Authorize)
		}
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
		end := phase(telemetry.PhasePrepare)
		err := opts.Prepare(ctx)
		end(err)
		if err != nil {
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
	policy, err := repoconfig.Load(ctx, root)
	if err != nil {
		fin(job, 0, 0, err)
		return
	}
	approved := ""
	if j.Policy != nil {
		approved, err = j.Policy(ctx, job.Repo, policy)
	}
	if err != nil {
		fin(job, 0, 0, err)
		return
	}
	ctx = repoconfig.WithPolicy(ctx, policy)
	endIngest := phase(telemetry.PhaseIngest)
	progress := func(step int, name string) {
		j.mu.Lock()
		job.Phase, job.Step, job.Steps = name, step, ingest.Steps+1
		j.mu.Unlock()
	}
	g, err := ingest.Run(ctx, ingest.Options{Progress: progress, Repo: job.Repo, Root: root, CommitLimit: j.commitLimit, AllowNetwork: j.network, VulnScan: j.vulnScan, LicenseLookup: j.licenseLookup, Dataflow: j.dataflow, ModuleProxy: j.moduleProxy, Branch: opts.Branch, PreviousIdentities: previous, ApprovedPolicyDigest: approved,
		Extracted: func(extractor string, d time.Duration) { telemetry.RecordExtract(ctx, extractor, d) }})
	endIngest(err)
	if err == nil {
		progress(ingest.Steps+1, "Saving the graph")
	}
	if err == nil && ledger != nil {
		report := ledger.Prepare(g)
		err = checkJob(ctx, opts.Authorize)
		if err == nil && j.Report != nil {
			err = j.writeReport(ctx, *job, report, opts.Authorize)
		}
		if err == nil {
			err = attachIngestReceipt(g, job.OrgID, job.Repo, job.ID)
		}
	}
	if err == nil {
		err = checkJob(ctx, opts.Authorize)
	}
	if err == nil {
		// Workers may not change the schema; the API created it at start.
		err = j.st.CheckSchema(ctx)
	}
	if err == nil {
		err = checkJob(ctx, opts.Authorize)
	}
	if err == nil {
		latest, loadErr := repoconfig.Load(ctx, root)
		if loadErr != nil {
			err = loadErr
		} else if latest.Digest() != policy.Digest() {
			err = errors.New("repository configuration changed during ingest")
		} else if j.Policy != nil {
			_, err = j.Policy(ctx, job.Repo, latest)
		}
	}
	if err == nil {
		indexed, _ := g.Node(model.NodeID(job.Repo, model.KindRepository, job.Repo))
		if indexed != nil && indexed.Head != gitx.HeadCommit(ctx, root) {
			err = errors.New("repository commit changed during ingest")
		}
	}
	if err == nil {
		ctx = store.WithCommitCheck(ctx, func(checkCtx context.Context) error {
			if err := checkJob(checkCtx, opts.Authorize); err != nil {
				return err
			}
			latest, err := repoconfig.Load(checkCtx, root)
			if err != nil {
				return err
			}
			if latest.Digest() != policy.Digest() {
				return errors.New("repository configuration changed before publication")
			}
			indexed, ok := g.Node(model.NodeID(job.Repo, model.KindRepository, job.Repo))
			if ok && indexed.Head != gitx.HeadCommit(checkCtx, root) {
				return errors.New("repository commit changed before publication")
			}
			if j.Policy != nil {
				_, err = j.Policy(checkCtx, job.Repo, latest)
			}
			return err
		})
		publish := func(publishCtx context.Context) error {
			graphAttempted = true
			if err := j.st.ReplaceRepo(publishCtx, job.Repo, g); err != nil {
				return err
			}
			if ledger != nil && j.Report != nil {
				report, err := ledger.Snapshot().Finish(ingestreport.Succeeded, ingestreport.CommitCommitted)
				if err != nil {
					return err
				}
				return j.writeReport(publishCtx, *job, report, opts.Authorize)
			}
			return nil
		}
		endPublish := phase(telemetry.PhasePublish)
		if j.Publish != nil {
			err = j.Publish(ctx, job.Repo, publish)
		} else {
			err = publish(ctx)
		}
		endPublish(err)
		reportPublished = err == nil
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
	if err == nil && j.Ingested != nil {
		j.Ingested(ctx, job.Repo, g, opts.Authorize)
	}
	if err == nil && opts.After != nil {
		// The hook queues its own work and must finish before this inherited
		// execution context is cancelled on return.
		if checkJob(ctx, opts.Authorize) == nil {
			opts.After(ctx)
		}
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
		j.logger.Error("job failed", "job", job.ID, "kind", job.Kind, "repo", job.Repo, "error", err)
		return
	}
	job.State = JobSucceeded
	// Every job logs how it began ("job started"); a success has to say how it
	// ended too, or a worker's log shows work that never seems to finish.
	j.logger.Info("job finished", "job", job.ID, "kind", job.Kind, "task", job.Task, "repo", job.Repo, "attempt", job.Attempts+1)
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

// endDurable records how a durable job ended.
func (j *Jobs) endDurable(ctx context.Context, job *Job, cause error) {
	j.mu.Lock()
	out := accounts.JobOutcome{State: accounts.JobSucceeded, Result: job.Result, Nodes: job.Nodes, Edges: job.Edges, At: time.Now().UTC()}
	j.mu.Unlock()
	if cause != nil {
		out.State, out.Error = accounts.JobFailed, cause.Error()
	}
	j.recordEnd(ctx, job.ID, out)
}

// recordEnd stores how a job ended, even when ctx was cancelled (shutdown).
func (j *Jobs) recordEnd(ctx context.Context, id string, out accounts.JobOutcome) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := j.store.FinishJob(ctx, id, j.owner, out); err != nil {
		j.logger.Error("job outcome not recorded", "job", id, "error", err)
	}
}

// Get returns a snapshot of one job.
func (j *Jobs) Get(ctx context.Context, id string) (Job, bool) {
	if j.store != nil {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		rec, err := j.store.JobByID(ctx, id)
		if err != nil {
			return Job{}, false
		}
		return fromRecord(rec), true
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *job, true
}

// List returns snapshots of all jobs, newest first.
func (j *Jobs) List(ctx context.Context) []Job {
	if j.store != nil {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		recs, err := j.store.ListJobs(ctx, nil, 500)
		if err != nil {
			j.logger.Warn("listing jobs", "error", err)
			return []Job{}
		}
		out := make([]Job, 0, len(recs))
		for _, r := range recs {
			out = append(out, fromRecord(r))
		}
		return out
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Job, 0, len(j.jobs))
	for _, job := range j.jobs {
		out = append(out, *job)
	}
	slices.SortFunc(out, func(a, b Job) int { return cmp.Or(cmp.Compare(b.StartedAt, a.StartedAt), cmp.Compare(a.ID, b.ID)) })
	return out
}
