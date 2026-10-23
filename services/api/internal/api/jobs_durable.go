package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// Durable jobs (docs/SAAS.md, 5e). With accounts, every job is a row in the
// accounts store before it runs. This process holds its jobs through a lease
// it renews while they wait or run; after a restart, resumeJobs claims jobs
// whose lease expired and starts them again through the same code that
// built them, with the requester's session and permission checked anew.

const (
	jobLease     = 2 * time.Minute
	jobHeartbeat = 30 * time.Second
	// jobPoll is how often an idle process looks for work.
	jobPoll = 2 * time.Second
)

// Job roles (docs/SAAS.md, 5e): which durable job kinds a process runs. The
// API with workers runs none and only records jobs; a worker runs its roles;
// a process on its own (development, tests) runs all of them.
var jobRoles = map[string][]string{
	"ingest": {"sync", "clone", "switch"},
	"ai":     {"ai", "ai-auto"},
	"fix":    {"fix"},
}

// JobRoles lists the roles a worker can take.
func JobRoles() []string { return []string{"ingest", "ai", "fix"} }

// kindsFor turns roles into job kinds; nil roles mean every kind.
func kindsFor(roles []string) ([]string, error) {
	if roles == nil {
		roles = JobRoles()
	}
	kinds := []string{}
	for _, r := range roles {
		k, ok := jobRoles[r]
		if !ok {
			return nil, fmt.Errorf("unknown job role %q (want ingest, ai or fix)", r)
		}
		kinds = append(kinds, k...)
	}
	return kinds, nil
}

// jobStore is what the runner needs from the accounts store.
type jobStore interface {
	AddJob(context.Context, accounts.JobRecord) error
	JobByID(context.Context, string) (accounts.JobRecord, error)
	ListJobs(context.Context, []string, int) ([]accounts.JobRecord, error)
	StartJob(ctx context.Context, id, owner string, leaseUntil, now time.Time) error
	RenewJobLeases(ctx context.Context, owner string, until time.Time) error
	FinishJob(ctx context.Context, id, owner string, out accounts.JobOutcome) error
	ClaimOrphanedJob(ctx context.Context, owner string, kinds []string, leaseUntil, now time.Time) (accounts.JobRecord, bool, error)
}

// durableJob describes a job well enough to rebuild it after a restart.
type durableJob struct {
	Kind    string
	Role    accounts.Role
	ActorID string
	Via     string
	KeyID   string
	Params  json.RawMessage
	// Trace continues the queueing request's trace in whichever process
	// runs the job.
	Trace string
	// resume is the record being started again; nil for a new job.
	resume *accounts.JobRecord
}

type resumeKey struct{}

// withResume marks ctx as starting rec again, so the job keeps its ID.
func withResume(ctx context.Context, rec accounts.JobRecord) context.Context {
	return context.WithValue(ctx, resumeKey{}, rec)
}

// durable describes a job for the store from the request (or resumed)
// context: who asked, how, and params to rebuild it with.
func durable(ctx context.Context, kind string, role accounts.Role, params any) (*durableJob, error) {
	p, ok := principal(ctx)
	if !ok {
		return nil, errors.New("a durable job needs an authenticated requester")
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	if len(raw) > accounts.MaxJobParams {
		return nil, fmt.Errorf("%w: job parameters are too large", errBadRequest)
	}
	d := &durableJob{Kind: kind, Role: role, ActorID: p.User.ID, Via: p.Via, KeyID: p.KeyID, Params: raw, Trace: telemetry.TraceParent(ctx)}
	if rec, ok := ctx.Value(resumeKey{}).(accounts.JobRecord); ok && rec.Kind == kind {
		d.resume, d.Trace = &rec, rec.Trace
	}
	return d, nil
}

// durableIf describes the job when jobs are durable (accounts), else nil.
func (s *Server) durableIf(ctx context.Context, kind string, role accounts.Role, params any) (*durableJob, error) {
	if s.jobs.store == nil {
		return nil, nil
	}
	return durable(ctx, kind, role, params)
}

func newOwner() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) //nolint:errcheck // crypto/rand does not fail on supported platforms
	return hex.EncodeToString(b)
}

// heartbeat renews this process's leases until ctx ends.
func (j *Jobs) heartbeat(ctx context.Context) {
	t := time.NewTicker(jobHeartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			work, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := j.store.RenewJobLeases(work, j.owner, time.Now().Add(jobLease)); err != nil {
				j.logger.Warn("renewing job leases", "error", err)
			}
			cancel()
		}
	}
}

// fromRecord is the job view of a stored record.
func fromRecord(r accounts.JobRecord) Job {
	job := Job{ID: r.ID, Repo: r.Repo, OrgID: r.OrgID, Identity: r.Execution, Kind: r.Kind, Branch: r.Branch, Task: r.Task,
		StartedAt: r.CreatedAt.Format(time.RFC3339), Result: r.Result, Error: r.Error, Nodes: r.Nodes, Edges: r.Edges, Attempts: r.Attempts}
	switch r.State {
	case accounts.JobQueued:
		job.State, job.Waiting = JobRunning, true
	case accounts.JobRunning:
		job.State = JobRunning
	case accounts.JobSucceeded:
		job.State = JobSucceeded
	default:
		job.State = JobFailed
	}
	if r.FinishedAt != nil {
		job.FinishedAt = r.FinishedAt.Format(time.RFC3339)
	}
	return job
}

// runQueue claims jobs of the kinds this process runs: those queued for
// workers and those a stopped process left behind. It claims only while
// idle, so several workers share the work, then starts each job through the
// function that built it, with the requester's context rebuilt and their
// session, account, key, membership and permission checked again (never
// trusted from the record). An interrupted fix is never restarted: a
// second run could push a second branch.
func (s *Server) runQueue(ctx context.Context) {
	t := time.NewTicker(jobPoll)
	defer t.Stop()
	for {
		for ctx.Err() == nil && s.jobs.idle() {
			now := time.Now().UTC()
			rec, found, err := s.jobs.store.ClaimOrphanedJob(ctx, s.jobs.owner, s.jobs.kinds, now.Add(jobLease), now)
			if err != nil {
				s.logger.Warn("claiming jobs", "error", err)
				break
			}
			if !found {
				break
			}
			what := "job started"
			if rec.Attempts > 0 {
				what = "interrupted job resumed"
			}
			if err := s.resumeJob(ctx, rec); err != nil {
				s.logger.Warn("job not started", "job", rec.ID, "kind", rec.Kind, "repo", rec.Repo, "error", err)
				reason := "not started: "
				if rec.Attempts > 0 {
					reason = "interrupted by a restart and not resumed: "
				}
				s.jobs.recordEnd(ctx, rec.ID, accounts.JobOutcome{State: accounts.JobFailed, At: time.Now().UTC(), Error: reason + err.Error()})
				continue
			}
			s.logger.Info(what, "job", rec.ID, "kind", rec.Kind, "repo", rec.Repo, "attempt", rec.Attempts+1)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) resumeJob(ctx context.Context, rec accounts.JobRecord) error {
	if rec.Kind == "fix" && rec.Attempts > 0 {
		return errors.New("fix branches are not restarted automatically; start the fix again")
	}
	jobCtx, err := s.jobContext(ctx, rec)
	if err != nil {
		return err
	}
	jobCtx = withResume(jobCtx, rec)
	switch rec.Kind {
	case "sync":
		var p syncParams
		if err := json.Unmarshal(rec.Params, &p); err != nil {
			return err
		}
		var done func(error)
		// Shared schedules (accounts) take a worker's report; a schedule
		// file belongs to the API process alone.
		if p.Scheduled && (s.sched.db != nil || !s.cfg.Worker) {
			repo := rec.Repo
			done = func(err error) { s.sched.finished(context.WithoutCancel(ctx), repo, time.Now(), err) }
		}
		_, err = s.startSync(jobCtx, rec.Repo, p.Branch, false, done)
	case "clone":
		var p cloneParams
		if err := json.Unmarshal(rec.Params, &p); err != nil {
			return err
		}
		if err := s.removePartialClone(rec.Repo); err != nil {
			return err
		}
		auth := gitx.Auth{}
		if p.Private {
			if auth, err = s.orgAuth(jobCtx, rec.OrgID, p.Remote); err != nil {
				return fmt.Errorf("the organisation's Git token is no longer available: %w", err)
			}
		}
		org, authorize := s.jobAuthorization(jobCtx, rec.Repo, accounts.Admin)
		d, derr := durable(jobCtx, "clone", accounts.Admin, p)
		if derr != nil {
			return derr
		}
		_, err = s.startCloneAuth(p.Remote, p.Name, p.Branch, auth, JobOptions{OrgID: org, Identity: identityOf(jobCtx), Authorize: authorize, Durable: d}) //nolint:contextcheck // jobs outlive the resume pass
	case "switch":
		var p switchParams
		if err := json.Unmarshal(rec.Params, &p); err != nil {
			return err
		}
		if err := s.removePartialClone(rec.Repo); err != nil {
			return err
		}
		auth, aerr := s.orgAuth(jobCtx, rec.OrgID, p.Remote)
		if aerr != nil {
			auth = gitx.Auth{} // public repositories need none
		}
		_, err = s.startSwitch(jobCtx, rec.Repo, repoDir(s.cfg.Workspace, rec.Repo), p.Remote, p.Branch, auth)
	case "ai":
		var p aiTaskParams
		if err := json.Unmarshal(rec.Params, &p); err != nil {
			return err
		}
		_, err = s.startAITask(jobCtx, rec.Repo, p)
	case "fix":
		var p fixParams
		if err := json.Unmarshal(rec.Params, &p); err != nil {
			return err
		}
		_, err = s.startFix(jobCtx, p)
	case "ai-auto":
		var p autoAIParams
		if err := json.Unmarshal(rec.Params, &p); err != nil {
			return err
		}
		err = s.startAutoAI(jobCtx, rec.Repo, p)
	default:
		return fmt.Errorf("unknown job kind %q", rec.Kind)
	}
	return err
}

// jobContext is the requester's context for a stored job, as the request had
// it: identity, principal and the one repository in scope, all checked again.
func (s *Server) jobContext(ctx context.Context, rec accounts.JobRecord) (context.Context, error) {
	if s.cfg.Accounts == nil {
		return nil, errExecutionRegistry
	}
	if rec.Execution.SubjectID != rec.ActorID || rec.Execution.TenantID != rec.OrgID {
		return nil, execution.ErrConflict
	}
	ctx, err := execution.With(ctx, rec.Execution)
	if err != nil {
		return nil, err
	}
	if _, err := s.cfg.Accounts.ResolveExecutionSession(ctx, rec.ActorID, rec.OrgID, rec.Execution.SessionID); err != nil {
		return nil, fmt.Errorf("the requester's session ended: %w", err)
	}
	u, err := s.cfg.Accounts.St.UserByID(ctx, rec.ActorID)
	if err != nil || u.DisabledAt != nil {
		return nil, errors.New("the requester's account is gone or suspended")
	}
	p := accounts.Principal{User: u, Via: rec.Via, KeyID: rec.KeyID}
	if rec.Via != "session" {
		// Keys and schedules act in one organisation, as when queued.
		p.OrgID = rec.OrgID
	}
	if rec.Via == "key" {
		if err := s.keyStillValid(ctx, rec); err != nil {
			return nil, err
		}
	}
	access, err := s.cfg.Accounts.ResolveRepository(ctx, p, rec.OrgID, rec.Repo, rec.Role)
	if err != nil {
		return nil, fmt.Errorf("the requester no longer has access: %w", err)
	}
	ctx = context.WithValue(ctx, principalKey{}, p)
	ctx = context.WithValue(ctx, scopeKey{}, scope{graphs: map[string]accounts.Role{rec.Repo: access.Role}, reach: map[string]accounts.Reach{rec.Repo: {OrgID: rec.OrgID, Role: access.Role}}})
	return ctx, nil
}

// keyStillValid refuses jobs whose API key was revoked or expired since.
func (s *Server) keyStillValid(ctx context.Context, rec accounts.JobRecord) error {
	keys, err := s.cfg.Accounts.St.ListAPIKeys(ctx, rec.ActorID)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, k := range keys {
		if k.ID == rec.KeyID {
			if k.RevokedAt != nil || (k.ExpiresAt != nil && !now.Before(*k.ExpiresAt)) || k.OrgID != rec.OrgID {
				return errors.New("the API key that started the job was revoked or expired")
			}
			return nil
		}
	}
	return errors.New("the API key that started the job is gone")
}

// removePartialClone deletes what an interrupted clone left in the
// workspace, so the clone can start again. Only a validated workspace child
// is removed; mounted sources are never touched.
func (s *Server) removePartialClone(repo string) error {
	if !validRepoName(repo) || s.cfg.Workspace == "" {
		return fmt.Errorf("%w: cannot restart a clone of %q", errBadRequest, repo)
	}
	dir := repoDir(s.cfg.Workspace, repo)
	if !s.inWorkspace(dir) {
		return fmt.Errorf("%w: %q is outside the workspace", errBadRequest, repo)
	}
	return os.RemoveAll(dir) //nolint:gosec // dir is a validated child of the workspace
}
