package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
)

// A clone that failed on the network (DNS, TLS, timeouts) leaves a repository
// that is linked to its organisation but has no graph, so the scheduler,
// which walks graphs, would never look at it again. recloner queues such
// clones again, waiting longer after each failure, and stops after
// recloneLimit tries so a repository that cannot be reached stays failed and
// visible instead of being hammered.
const recloneLimit = 8

var recloneBackoff = func(tries int) time.Duration {
	return min(time.Duration(1<<tries)*15*time.Minute, 6*time.Hour)
}

type recloner struct {
	mu    sync.Mutex
	tries map[string]int
}

// retryFailedClones queues a new clone for each repository whose latest clone
// job failed with a network error and that has no graph. The requester's
// session, membership and permission are checked again by jobContext.
func (s *Server) retryFailedClones(ctx context.Context, now time.Time, logger *slog.Logger) {
	if s.cfg.Accounts == nil {
		return
	}
	for repo, j := range s.latestJobs(ctx) {
		if j.Kind != "clone" || j.State != JobFailed || !gitx.TransientMessage(j.Error) {
			s.reclone.forget(repo)
			continue
		}
		finished, err := time.Parse(time.RFC3339, j.FinishedAt)
		tries := s.reclone.count(repo)
		if err != nil || tries >= recloneLimit || now.Sub(finished) < recloneBackoff(tries) {
			continue
		}
		if _, err := s.st.Node(ctx, model.NodeID(repo, model.KindRepository, repo)); !errors.Is(err, store.ErrNotFound) {
			continue // indexed since, or the store is down: not ours to restart
		}
		rec, err := s.jobs.store.JobByID(ctx, j.ID)
		if err != nil {
			logger.Warn("reading failed clone for retry", "repo", repo, "job", j.ID, "error", err)
			continue
		}
		// A finished job keeps no parameters, so the clone is described again
		// from the organisation's entry for the repository.
		if rec.Params, err = s.cloneParamsOf(ctx, rec); err != nil {
			logger.Warn("clone retry not started", "repo", repo, "job", j.ID, "error", err)
			continue
		}
		s.reclone.bump(repo)
		jobCtx, err := s.jobContext(ctx, rec)
		if err == nil {
			err = s.startRecordedClone(jobCtx, rec)
		}
		if err != nil {
			logger.Warn("clone retry not started", "repo", repo, "job", j.ID, "tries", tries+1, "error", err)
			continue
		}
		logger.Info("queued a new clone after a network failure", "repo", repo, "previous_job", j.ID, "tries", tries+1, "error", j.Error)
	}
}

// cloneParamsOf rebuilds the parameters of rec's clone from the repository
// entry its organisation still holds.
func (s *Server) cloneParamsOf(ctx context.Context, rec accounts.JobRecord) ([]byte, error) {
	entries, err := s.cfg.Accounts.St.OrgRepositories(ctx, rec.OrgID)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Graph == rec.Repo && e.Remote != "" && e.Access != accounts.AccessLost {
			return json.Marshal(cloneParams{Remote: e.Remote, Name: rec.Repo, Private: e.Visibility == accounts.Private})
		}
	}
	return nil, errors.New("the repository is no longer linked to the organisation")
}

func (r *recloner) count(repo string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tries[repo]
}

func (r *recloner) bump(repo string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tries == nil {
		r.tries = map[string]int{}
	}
	r.tries[repo]++
}

func (r *recloner) forget(repo string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tries, repo)
}
