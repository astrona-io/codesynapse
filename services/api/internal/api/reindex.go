package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/model"
)

// The scheduler only syncs a repository when it has a new commit, so a
// repository nobody pushes to would keep a graph from an older analysis
// forever. reindexOutdated re-indexes graphs whose model.IndexVersion is older
// than this build's, a few per tick so an upgrade does not queue every
// repository at once, as the member its schedule runs as.
const reindexPerTick = 2

// reindexRetry is how long a repository waits before another try when its
// re-index could not start or did not raise the version (a worker of an older
// build ran it during an upgrade, for instance).
const reindexRetry = 6 * time.Hour

type reindexer struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// try reports whether repo may be re-indexed at now and, if so, records the try.
func (r *reindexer) try(repo string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if at, ok := r.last[repo]; ok && now.Sub(at) < reindexRetry {
		return false
	}
	if r.last == nil {
		r.last = map[string]time.Time{}
	}
	r.last[repo] = now
	return true
}

// forget drops repositories that are current or gone, so the map stays bounded
// by the outdated ones.
func (r *reindexer) forget(keep map[string]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for repo := range r.last {
		if !keep[repo] {
			delete(r.last, repo)
		}
	}
}

// reindexOutdated starts a full sync of up to reindexPerTick repositories whose
// graph an older IndexVersion wrote. Only repositories with an active schedule
// are considered (a paused one stays as it is), and each re-index is audited as
// repo.reindex before it starts; a failed audit starts nothing.
func (s *Server) reindexOutdated(ctx context.Context, repos []model.Node, now time.Time, logger *slog.Logger) {
	if s.cfg.Accounts == nil {
		return
	}
	outdated := map[string]bool{}
	var jobs map[string]Job
	started := 0
	for _, n := range repos {
		from := model.IndexVersionOf(n)
		if from >= model.IndexVersion {
			continue
		}
		outdated[n.Name] = true
		if started >= reindexPerTick {
			continue
		}
		sc := s.sched.get(ctx, n.Name)
		if sc.State() != "active" {
			continue
		}
		if jobs == nil {
			jobs = s.latestJobs(ctx)
		}
		if j, ok := jobs[n.Name]; ok && j.State == JobRunning {
			continue // the running ingest writes the current version
		}
		if !s.reindex.try(n.Name, now) {
			continue
		}
		started++
		if err := s.startReindex(ctx, sc, from); err != nil {
			logger.Warn("re-index of an outdated graph not started", "repo", n.Name, "index_version", from, "error", err)
			continue
		}
		logger.Info("re-indexing a graph from an older analysis", "repo", n.Name, "index_version", from, "current", model.IndexVersion)
	}
	s.reindex.forget(outdated)
}

// startReindex audits and starts one re-index as the schedule's member. A
// start that fails after the audit is recorded as a failed repo.reindex.
func (s *Server) startReindex(ctx context.Context, sc Schedule, from int) error {
	jobCtx, err := s.scheduleContext(ctx, sc)
	if err != nil {
		return err
	}
	ev := accounts.Event{UserID: sc.ActorID, Via: "automation", OrgID: sc.OrgID, Action: "repo.reindex", Target: sc.Repo, Outcome: accounts.Success,
		Detail: fmt.Sprintf("analysis version %d is older than %d", from, model.IndexVersion)}
	if _, err := s.cfg.Accounts.Record(ctx, ev); err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	_, err = s.startSync(jobCtx, sc.Repo, "", false, func(err error) { s.sched.finished(context.WithoutCancel(ctx), sc.Repo, time.Now(), err) })
	if err != nil {
		ev.Outcome, ev.Detail = accounts.Failure, "not started: "+err.Error()
		if _, aerr := s.cfg.Accounts.Record(ctx, ev); aerr != nil {
			err = errors.Join(err, fmt.Errorf("audit: %w", aerr))
		}
	}
	return err
}
