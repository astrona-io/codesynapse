package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/accounts"
	"github.com/parisnakitakejser/codesynapse/internal/gitx"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/reviews"
	"github.com/parisnakitakejser/codesynapse/services/api/internal/fixes"
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
	n, err := s.st.Node(r.Context(), model.NodeID(rv.Repo, model.KindRepository, rv.Repo))
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	if !s.inWorkspace(n.Root) {
		s.problem(w, 400, errors.New("fix branches need a repository added by URL; mounted sources are never pushed from"))
		return
	}
	if n.Branch == "" || n.Head == "" {
		s.problem(w, 400, errors.New("sync the repository first: its branch and head commit are unknown"))
		return
	}
	remote, err := gitx.ValidateRemote(r.Context(), n.URL, gitx.RemoteOptions{AllowPrivate: s.cfg.AllowPrivateGit})
	if err != nil {
		s.problem(w, 400, err)
		return
	}
	auth, err := s.pushAuth(r, rv.Repo, remote)
	if err != nil {
		s.problem(w, http.StatusForbidden, err)
		return
	}
	org, authorize := s.jobAuthorization(r.Context(), rv.Repo, accounts.Member)
	runner := fixes.Runner{Authorize: authorize, St: s.st, LLM: s.cfg.LLM, Auth: auth, Author: s.cfg.GitAuthor,
		Workdir: filepath.Join(s.cfg.Workspace, ".codesynapse", "fixes")}
	in := fixes.Request{ReviewID: rv.ID, CommentIDs: req.CommentIDs, Provider: req.Provider, Root: n.Root, Remote: remote, Base: n.Head, BaseBranch: n.Branch}
	job, err := s.jobs.Start(rv.Repo, "", JobOptions{ //nolint:contextcheck // jobs outlive the request
		Kind: "fix", Task: fixes.Task, OrgID: org, Authorize: authorize,
		Run: func(ctx context.Context) (string, error) {
			res, err := runner.Run(ctx, in)
			if res.PRURL != "" {
				return res.PRURL, err
			}
			return res.Branch, err
		},
	})
	if err != nil {
		s.problem(w, http.StatusConflict, err)
		return
	}
	// Shown until the job gets its turn and the runner marks it running.
	reviews.SetFix(&rv, req.CommentIDs, reviews.Fix{State: reviews.FixQueued, Job: job.ID}, time.Now())
	if err := s.st.SaveReview(r.Context(), rv); err != nil {
		s.logger.Warn("fix queued state not saved", "review", rv.ID, "error", err)
	}
	writeJSON(w, http.StatusAccepted, job)
}

// recoverFixes marks fixes that were queued or running when the API stopped
// as failed: their jobs lived in memory and are gone, so nothing will finish
// them.
func (s *Server) recoverFixes(ctx context.Context) {
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
				if c.Fix != nil && (c.Fix.State == reviews.FixQueued || c.Fix.State == reviews.FixRunning) {
					ids = append(ids, c.ID)
				}
			}
			if len(ids) == 0 {
				continue
			}
			reviews.SetFix(&rv, ids, reviews.Fix{State: reviews.FixFailed, Error: "Interrupted: CodeSynapse restarted before the fix finished. Try again."}, time.Now())
			if err := s.st.SaveReview(ctx, rv); err == nil {
				s.logger.Info("interrupted fixes marked failed", "review", rv.ID, "comments", len(ids))
			}
		}
	}
}
