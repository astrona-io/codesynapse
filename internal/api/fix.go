package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/parisnakitakejser/codesynapse/internal/fixes"
	"github.com/parisnakitakejser/codesynapse/internal/gitx"
	"github.com/parisnakitakejser/codesynapse/internal/model"
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
	runner := fixes.Runner{St: s.st, LLM: s.cfg.LLM, Auth: s.cfg.GitAuth, Author: s.cfg.GitAuthor,
		Workdir: filepath.Join(s.cfg.Workspace, ".codesynapse", "fixes")}
	in := fixes.Request{ReviewID: rv.ID, CommentIDs: req.CommentIDs, Provider: req.Provider, Root: n.Root, Remote: remote, Base: n.Head, BaseBranch: n.Branch}
	job, err := s.jobs.Start(rv.Repo, "", JobOptions{ //nolint:contextcheck // jobs outlive the request
		Kind: "fix", Task: fixes.Task,
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
	writeJSON(w, http.StatusAccepted, job)
}
