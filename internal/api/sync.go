package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/parisnakitakejser/codesynapse/internal/gitx"
	"github.com/parisnakitakejser/codesynapse/internal/model"
)

// inWorkspace reports whether root is a clone managed by the API.
func (s *Server) inWorkspace(root string) bool {
	if s.cfg.Workspace == "" || root == "" {
		return false
	}
	rel, err := filepath.Rel(s.cfg.Workspace, root)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..")
}

type registerRequest struct {
	URL    string `json:"url"`
	Name   string `json:"name"`
	Branch string `json:"branch"`
}

// handleRegister clones a remote repository into the workspace and indexes it.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Workspace == "" {
		s.problem(w, http.StatusNotImplemented, errors.New("no workspace configured (CODESYNAPSE_WORKSPACE)"))
		return
	}
	var req registerRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, 400, err)
		return
	}
	remote, err := gitx.ValidateRemote(r.Context(), req.URL, gitx.RemoteOptions{AllowPrivate: s.cfg.AllowPrivateGit})
	if err != nil {
		s.problem(w, 400, err)
		return
	}
	if req.Name == "" {
		req.Name = strings.TrimSuffix(filepath.Base(remote), ".git")
	}
	if !validRepoName(req.Name) {
		s.problem(w, 400, errors.New("name must match [A-Za-z0-9._-]+"))
		return
	}
	if req.Branch != "" {
		if err := gitx.ValidateRev(req.Branch); err != nil {
			s.problem(w, 400, err)
			return
		}
	}
	dir := filepath.Join(s.cfg.Workspace, req.Name)
	if _, err := os.Stat(dir); err == nil {
		s.problem(w, http.StatusConflict, fmt.Errorf("%q already exists; use sync", req.Name))
		return
	}
	auth := s.cfg.GitAuth
	job, err := s.jobs.Start(req.Name, dir, JobOptions{ //nolint:contextcheck // jobs outlive the request
		Kind: "clone", Branch: req.Branch,
		Prepare: func(ctx context.Context) error {
			if err := gitx.Clone(ctx, remote, req.Branch, dir, auth); err != nil {
				_ = os.RemoveAll(dir) //nolint:errcheck // best-effort cleanup of a partial clone
				return err
			}
			return nil
		},
	})
	if err != nil {
		s.problem(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

type syncRequest struct {
	Branch string `json:"branch"`
}

// Errors from startSync, mapped to HTTP statuses by handleSync.
var (
	errBadRequest = errors.New("bad request")
	errConflict   = errors.New("conflict")
)

// handleSync updates a repository and re-indexes it.
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	var req syncRequest
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &req); err != nil {
			s.problem(w, 400, err)
			return
		}
	}
	job, err := s.startSync(r.Context(), r.PathValue("repo"), req.Branch)
	switch {
	case errors.Is(err, errBadRequest):
		s.problem(w, 400, err)
	case errors.Is(err, errConflict):
		s.problem(w, http.StatusConflict, err)
	case err != nil:
		s.problem(w, 500, err)
	default:
		writeJSON(w, http.StatusAccepted, job)
	}
}

// startSync queues a sync of repo. Clones in the workspace fetch branch
// (default: the indexed one) from origin; mounted sources are re-indexed as
// they are on disk. Used by the Sync button and the scheduler.
func (s *Server) startSync(ctx context.Context, repo, branch string) (Job, error) {
	n, err := s.st.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		return Job{}, err
	}
	requested := branch
	if branch == "" {
		branch = n.Branch
	}
	opts := JobOptions{Kind: "sync", Branch: branch}
	switch {
	case s.inWorkspace(n.Root):
		if branch == "" {
			return Job{}, fmt.Errorf("%w: branch is required", errBadRequest)
		}
		if err := gitx.ValidateRev(branch); err != nil {
			return Job{}, fmt.Errorf("%w: %w", errBadRequest, err)
		}
		remote, err := gitx.ValidateRemote(ctx, n.URL, gitx.RemoteOptions{AllowPrivate: s.cfg.AllowPrivateGit})
		if err != nil {
			return Job{}, fmt.Errorf("%w: %w", errBadRequest, err)
		}
		root, auth := n.Root, s.cfg.GitAuth
		opts.Prepare = func(ctx context.Context) error { return gitx.FetchCheckout(ctx, root, remote, branch, auth) }
	default:
		if err := s.insideSources(n.Root); err != nil {
			return Job{}, fmt.Errorf("%w: cannot sync %q: %w", errBadRequest, repo, err)
		}
		if requested != "" && requested != n.Branch {
			return Job{}, fmt.Errorf("%w: mounted sources follow the checkout on disk; switch branches there, or register the repository by URL", errBadRequest)
		}
	}
	job, err := s.jobs.Start(repo, n.Root, opts) //nolint:contextcheck // jobs outlive the request
	if err != nil {
		return Job{}, fmt.Errorf("%w: %w", errConflict, err)
	}
	return job, nil
}

// handleBranches lists branches: remote heads for workspace clones, local
// branches for mounted sources.
func (s *Server) handleBranches(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	n, err := s.st.Node(r.Context(), model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	var branches []string
	managed := s.inWorkspace(n.Root)
	if managed {
		remote, err := gitx.ValidateRemote(r.Context(), n.URL, gitx.RemoteOptions{AllowPrivate: s.cfg.AllowPrivateGit})
		if err != nil {
			s.problem(w, 400, err)
			return
		}
		branches, err = gitx.RemoteBranches(r.Context(), remote, s.cfg.GitAuth)
		if err != nil {
			s.problem(w, http.StatusBadGateway, err)
			return
		}
	} else if s.insideSources(n.Root) == nil {
		branches, _ = gitx.LocalBranches(r.Context(), n.Root) //nolint:errcheck // a source without git has no branches
	}
	if branches == nil {
		branches = []string{}
	}
	writeJSON(w, 200, map[string]any{"current": n.Branch, "branches": branches, "managed": managed, "remote": n.URL})
}
