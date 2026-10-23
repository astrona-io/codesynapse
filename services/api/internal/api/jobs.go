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

	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/logx"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/telemetry"

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
	job := &Job{ID: hex.EncodeToString(b), Repo: repo, OrgID: opts.OrgID, Identity: opts.Identity, Kind: kind, Branch: opts.Branch, State: JobRunning, StartedAt: time.Now().UTC().Format(time.RFC3339), Task: opts.Task, Waiting: true}
	var ledger *ingestreport.Ledger
	if opts.Run == nil {
		ledger, err = ingestreport.New(job.ID, time.Now().UTC())
		if err != nil {
			return Job{}, err
		}
		if j.Report != nil {
			reportCtx, reportCancel := context.WithTimeout(ctx, 10*time.Second)
			err := j.writeReport(reportCtx, *job, ledger.Snapshot(), opts.Authorize)
			reportCancel()
			if err != nil {
				return Job{}, err
			}
		}
	}
	j.jobs[job.ID] = job
	j.prune()
	result := *job
	go j.run(job, root, opts, ledger)
	return result, nil
}

func (j *Jobs) run(job *Job, root string, opts JobOptions, ledger *ingestreport.Ledger) {
	ctx, cancel := context.WithTimeout(j.ctx, time.Hour)
	defer cancel()
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
	g, err := ingest.Run(ctx, ingest.Options{Repo: job.Repo, Root: root, CommitLimit: j.commitLimit, AllowNetwork: j.network, VulnScan: j.vulnScan, LicenseLookup: j.licenseLookup, Dataflow: j.dataflow, Branch: opts.Branch, PreviousIdentities: previous, ApprovedPolicyDigest: approved})
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
		err = j.st.EnsureSchema(ctx)
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
		if j.Publish != nil {
			err = j.Publish(ctx, job.Repo, publish)
		} else {
			err = publish(ctx)
		}
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
