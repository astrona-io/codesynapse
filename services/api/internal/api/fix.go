package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/reviews"
	"github.com/astrona-io/codesynapse/services/api/internal/fixes"
)

type fixRequest struct {
	CommentIDs []string `json:"comment_ids"`
	Provider   string   `json:"provider"`
}

// handleFixReview queues a job that fixes accepted comments on a new branch,
// pushes it and opens a draft pull request. The job result is the pull
// request URL, or the branch when no pull request could be opened.
func (s *Server) handleFixReview(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.AllowPush {
		s.problem(w, http.StatusConflict, errors.New("fix branches are off; set CODESYNAPSE_ALLOW_PUSH=true and use a git token with write access"))
		return
	}
	if s.cfg.LLM == nil {
		s.problem(w, http.StatusConflict, errors.New("no AI providers configured"))
		return
	}
	var req fixRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, 400, err)
		return
	}
	if len(req.CommentIDs) == 0 || len(req.CommentIDs) > 20 {
		s.problem(w, 400, errors.New("comment_ids: give 1 to 20 accepted comments"))
		return
	}
	rv, err := s.st.GetReview(r.Context(), r.PathValue("id"))
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	for _, c := range rv.Comments {
		for _, id := range req.CommentIDs {
			if c.ID == id {
				if why := fixes.Selectable(c); why != "" {
					s.problem(w, 400, fmt.Errorf("%s:%d: %s", c.File, c.Line, why))
					return
				}
			}
		}
	}
	if _, err := s.cfg.LLM.ProviderFor(fixes.Task, req.Provider); err != nil {
		s.problem(w, 400, err)
		return
	}
	job, err := s.startFix(r.Context(), fixParams{ReviewID: rv.ID, CommentIDs: req.CommentIDs, Provider: req.Provider}) //nolint:contextcheck // jobs outlive the request
	if err != nil {
		s.jobError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

// fixParams rebuild a fix job in a worker (jobs_durable.go).
type fixParams struct {
	ReviewID   string   `json:"review"`
	CommentIDs []string `json:"comments"`
	Provider   string   `json:"provider,omitempty"`
}

// startFix queues a fix of p's comments as the requester in ctx. Everything
// is checked again from the stored review and repository: the review must
// still match the indexed head, and the push credentials are the
// organisation's own.
func (s *Server) startFix(ctx context.Context, p fixParams) (Job, error) {
	rv, err := s.st.GetReview(ctx, p.ReviewID)
	if err != nil {
		return Job{}, err
	}
	n, err := s.st.Node(ctx, model.NodeID(rv.Repo, model.KindRepository, rv.Repo))
	if err != nil {
		return Job{}, err
	}
	if !s.inWorkspace(n.Root) {
		return Job{}, fmt.Errorf("%w: fix branches need a repository added by URL; mounted sources are never pushed from", errBadRequest)
	}
	if n.Branch == "" || n.Head == "" {
		return Job{}, fmt.Errorf("%w: sync the repository first: its branch and head commit are unknown", errBadRequest)
	}
	if err := fixes.ValidateRevision(rv, n.Head); err != nil {
		return Job{}, fmt.Errorf("%w: %w", errConflict, err)
	}
	remote, err := gitx.ValidateRemote(ctx, n.URL, gitx.RemoteOptions{AllowPrivate: s.cfg.AllowPrivateGit})
	if err != nil {
		return Job{}, fmt.Errorf("%w: %w", errBadRequest, err)
	}
	auth, err := s.pushAuthFor(ctx, rv.Repo, remote)
	if err != nil {
		return Job{}, fmt.Errorf("%w: %w", accounts.ErrForbidden, err)
	}
	org, authorize := s.jobAuthorization(ctx, rv.Repo, accounts.Member)
	runner := fixes.Runner{Authorize: authorize, St: s.st, LLM: s.cfg.LLM, Auth: auth, Author: s.cfg.GitAuthor,
		Workdir: filepath.Join(s.cfg.Workspace, ".codesynapse", "fixes"),
		CurrentRevision: func(ctx context.Context) (string, error) {
			current, err := s.st.Node(ctx, model.NodeID(rv.Repo, model.KindRepository, rv.Repo))
			return current.Head, err
		},
	}
	in := fixes.Request{ValidationVersion: fixes.ValidationVersion, ReviewID: rv.ID, CommentIDs: p.CommentIDs, Provider: p.Provider, Root: n.Root, Remote: remote, Base: n.Head, BaseBranch: n.Branch}
	d, err := s.durableIf(ctx, "fix", accounts.Member, p)
	if err != nil {
		return Job{}, err
	}
	job, err := s.jobs.Start(rv.Repo, "", JobOptions{ //nolint:contextcheck // jobs outlive the request
		Kind: "fix", Task: fixes.Task, OrgID: org, Identity: identityOf(ctx), Authorize: authorize, Durable: d,
		Run: func(ctx context.Context) (string, error) {
			res, err := runner.Run(ctx, in)
			if res.PRURL != "" {
				return res.PRURL, err
			}
			return res.Branch, err
		},
	})
	if err != nil {
		return Job{}, fmt.Errorf("%w: %w", errConflict, err)
	}
	if d == nil || d.resume == nil {
		// Shown until the job gets its turn and the runner marks it running.
		reviews.SetFix(&rv, p.CommentIDs, reviews.Fix{State: reviews.FixQueued, Job: job.ID}, time.Now())
		if err := s.st.SaveReview(ctx, rv); err != nil {
			s.logger.Warn("fix queued state not saved", "review", rv.ID, "error", err)
		}
	}
	return job, nil
}

// jobError maps errors from the start functions to statuses.
func (s *Server) jobError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errBadRequest):
		s.problem(w, http.StatusBadRequest, err)
	case errors.Is(err, accounts.ErrForbidden):
		s.problem(w, http.StatusForbidden, err)
	case errors.Is(err, errConflict):
		s.problem(w, http.StatusConflict, err)
	default:
		s.problem(w, http.StatusInternalServerError, err)
	}
}

// recoverFixes marks fixes as failed whose job will never finish them. Fix
// jobs are never restarted (a second run could push twice). Without durable
// jobs every queued or running fix died with the process, so this runs once
// at start-up; with them, a fix is stuck only when its job record has ended
// (a worker may still be running it), so this checks every minute.
func (s *Server) recoverFixes(ctx context.Context) {
	if s.jobs == nil || s.jobs.store == nil {
		s.failStuckFixes(ctx, func(string) bool { return true })
		return
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		s.failStuckFixes(ctx, func(job string) bool {
			rec, err := s.jobs.store.JobByID(ctx, job)
			return err == nil && !rec.Active()
		})
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) failStuckFixes(ctx context.Context, stuck func(job string) bool) {
	repos, err := s.st.Repos(ctx)
	if err != nil {
		return
	}
	for _, n := range repos {
		list, err := s.st.ListReviews(ctx, n.Name, 500)
		if err != nil {
			continue
		}
		for _, rv := range list {
			var ids []string
			for _, c := range rv.Comments {
				if c.Fix != nil && (c.Fix.State == reviews.FixQueued || c.Fix.State == reviews.FixRunning) && stuck(c.Fix.Job) {
					ids = append(ids, c.ID)
				}
			}
			if len(ids) == 0 {
				continue
			}
			reviews.SetFix(&rv, ids, reviews.Fix{State: reviews.FixFailed, Error: "Interrupted: the fix did not finish (CodeSynapse restarted or the job ended). Try again."}, time.Now())
			if err := s.st.SaveReview(ctx, rv); err == nil {
				s.logger.Info("interrupted fixes marked failed", "review", rv.ID, "comments", len(ids))
			}
		}
	}
}
