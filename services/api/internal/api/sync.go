package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/gitx"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/store"
	"github.com/parisnakitakejser/codesynapse/services/api/internal/aitasks"
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
		req.Name = repoNameFromRemote(remote)
	}
	if !validRepoName(req.Name) {
		s.problem(w, 400, errors.New("name must be name or owner/name using only [A-Za-z0-9._-]"))
		return
	}
	if req.Branch != "" {
		if err := gitx.ValidateRev(req.Branch); err != nil {
			s.problem(w, 400, err)
			return
		}
	}
	job, err := s.startClone(remote, req.Name, req.Branch) //nolint:contextcheck // jobs outlive the request
	switch {
	case errors.Is(err, errConflict):
		s.problem(w, http.StatusConflict, err)
	case err != nil:
		s.problem(w, 500, err)
	default:
		writeJSON(w, http.StatusAccepted, job)
	}
}

// startClone queues a clone of an already validated remote into the
// workspace under name, then indexes it.
func (s *Server) startClone(remote, name, branch string) (Job, error) {
	dir := repoDir(s.cfg.Workspace, name)
	if _, err := os.Stat(dir); err == nil { //nolint:gosec // name passed validRepoName: at most two safe elements, none with a leading dot
		return Job{}, fmt.Errorf("%w: %q already exists; use sync", errConflict, name)
	}
	auth := s.cfg.GitAuth
	job, err := s.jobs.Start(name, dir, JobOptions{
		Kind: "clone", Branch: branch,
		Prepare: func(ctx context.Context) error {
			if err := gitx.Clone(ctx, remote, branch, dir, auth); err != nil {
				_ = os.RemoveAll(dir) //nolint:errcheck,gosec // best-effort cleanup of a partial clone; dir is workspace/<validated name>
				return err
			}
			return nil
		},
	})
	if err != nil {
		return Job{}, fmt.Errorf("%w: %w", errConflict, err)
	}
	return job, nil
}

type switchRequest struct {
	URL    string `json:"url"`
	Branch string `json:"branch"`
}

// handleSwitchToRemote turns a mounted repository into a clone managed by
// CodeSynapse: it clones the remote (default: the checkout's origin, ssh
// rewritten to https) into the workspace under the same name and re-indexes
// it there. Reviews and the schedule are keyed by name, so they carry over;
// the mounted folder is not touched. Clones can sync branches and push fix
// branches.
func (s *Server) handleSwitchToRemote(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Workspace == "" {
		s.problem(w, http.StatusNotImplemented, errors.New("no workspace configured (CODESYNAPSE_WORKSPACE)"))
		return
	}
	repo := r.PathValue("repo")
	n, err := s.st.Node(r.Context(), model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	if s.inWorkspace(n.Root) {
		s.problem(w, 400, fmt.Errorf("%q is already cloned from %s", repo, gitx.SanitizeURL(n.URL)))
		return
	}
	var req switchRequest
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &req); err != nil {
			s.problem(w, 400, err)
			return
		}
	}
	if !validRepoName(repo) {
		s.problem(w, 400, errors.New("repository name must be name or owner/name using only [A-Za-z0-9._-] to be cloned"))
		return
	}
	dir := repoDir(s.cfg.Workspace, repo)
	if _, err := os.Stat(dir); err == nil { //nolint:gosec // repo passed validRepoName: at most two safe elements, none with a leading dot
		s.problem(w, http.StatusConflict, fmt.Errorf("the workspace already has a folder named %q; remove it first", repo))
		return
	}
	if req.URL == "" {
		req.URL = gitx.SanitizeURL(gitx.HTTPSRemote(n.URL))
	}
	if req.URL == "" {
		s.problem(w, 400, errors.New("this checkout has no origin remote; enter the repository URL"))
		return
	}
	remote, err := gitx.ValidateRemote(r.Context(), req.URL, gitx.RemoteOptions{AllowPrivate: s.cfg.AllowPrivateGit})
	if err != nil {
		s.problem(w, 400, err)
		return
	}
	if req.Branch == "" {
		req.Branch = n.Branch
	}
	if req.Branch != "" {
		if err := gitx.ValidateRev(req.Branch); err != nil {
			s.problem(w, 400, err)
			return
		}
	}
	auth, branch := s.cfg.GitAuth, req.Branch
	job, err := s.jobs.Start(repo, dir, JobOptions{ //nolint:contextcheck // jobs outlive the request
		Kind: "clone", Branch: branch,
		Prepare: func(ctx context.Context) error {
			if err := gitx.Clone(ctx, remote, branch, dir, auth); err != nil {
				_ = os.RemoveAll(dir) //nolint:errcheck,gosec // best-effort cleanup; dir is a validated workspace child
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
	// errUnchanged means a scheduled sync found nothing new and queued no job.
	errUnchanged = errors.New("up to date")
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
	job, err := s.startSync(r.Context(), r.PathValue("repo"), req.Branch, false)
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
// they are on disk. Used by the Sync button and the scheduler. With
// onlyIfChanged (the scheduler) it first asks cheaply whether anything is new
// and returns errUnchanged without queueing a job when not: with hundreds of
// repositories every few minutes, a fetch and re-index each would never drain.
func (s *Server) startSync(ctx context.Context, repo, branch string, onlyIfChanged bool) (Job, error) {
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
		if onlyIfChanged {
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			head, err := gitx.RemoteHead(cctx, remote, branch, auth)
			cancel()
			if err != nil {
				return Job{}, fmt.Errorf("checking %s for new commits: %w", repo, err)
			}
			if head == "" || head == n.Head {
				return Job{}, errUnchanged
			}
		}
		opts.Prepare = func(ctx context.Context) error { return gitx.FetchCheckout(ctx, root, remote, branch, auth) }
	default:
		if err := s.insideSources(n.Root); err != nil {
			return Job{}, fmt.Errorf("%w: cannot sync %q: %w", errBadRequest, repo, err)
		}
		if requested != "" && requested != n.Branch {
			return Job{}, fmt.Errorf("%w: mounted sources follow the checkout on disk; switch branches there, or register the repository by URL", errBadRequest)
		}
		// A git checkout with the indexed commit and no local edits is unchanged.
		if onlyIfChanged && n.Head != "" && gitx.HeadCommit(ctx, n.Root) == n.Head && !gitx.IsDirty(ctx, n.Root) {
			return Job{}, errUnchanged
		}
	}
	opts.After = s.automation(ctx, repo, n)
	job, err := s.jobs.Start(repo, n.Root, opts) //nolint:contextcheck // jobs outlive the request
	if err != nil {
		return Job{}, fmt.Errorf("%w: %w", errConflict, err)
	}
	return job, nil
}

// automation returns the post-sync hook for repo, or nil when no automation
// is enabled. It snapshots the current head and findings now, so the hook can
// review exactly the commits and triage exactly the findings that are new.
func (s *Server) automation(ctx context.Context, repo string, before model.Node) func(context.Context) {
	sc := s.sched.get(repo)
	if s.cfg.LLM == nil || (!sc.AutoReview && !sc.AutoTriage) {
		return nil
	}
	oldFindings := map[string]bool{}
	if list, err := s.st.List(ctx, repo, store.Filter{Kinds: []model.NodeKind{model.KindFinding}}, store.ByName, store.MaxLimit); err == nil {
		for _, f := range list {
			oldFindings[f.Node.ID] = true
		}
	}
	oldHead := before.Head
	return func(ctx context.Context) {
		after, err := s.st.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
		if err != nil {
			return
		}
		runner := aitasks.Runner{St: s.st, LLM: s.cfg.LLM}
		var newFindings []string
		if sc.AutoTriage {
			if list, err := s.st.List(ctx, repo, store.Filter{Kinds: []model.NodeKind{model.KindFinding}}, store.ByName, store.MaxLimit); err == nil {
				for _, f := range list {
					if !oldFindings[f.Node.ID] && model.SeverityRank(f.Node.Severity) >= model.SeverityRank("medium") {
						newFindings = append(newFindings, f.Node.ID)
					}
				}
			}
		}
		var diff []byte
		if sc.AutoReview && oldHead != "" && after.Head != "" && oldHead != after.Head {
			if d, err := gitx.Diff(ctx, after.Root, oldHead, after.Head); err == nil {
				diff = d
			} else {
				s.logger.Warn("auto-review: diff of new commits failed", "repo", repo, "error", err)
			}
		}
		if len(newFindings) == 0 && len(diff) == 0 {
			return
		}
		_, err = s.jobs.Start(repo, "", JobOptions{Kind: "ai", Task: "automatic", Run: func(ctx context.Context) (string, error) { //nolint:contextcheck // runs as its own job
			var last string
			var errs []error
			if len(newFindings) > 0 {
				id, err := runner.Run(ctx, "security-triage", aitasks.Input{Repo: repo, OnlyFindings: newFindings})
				if err == nil {
					last = id
				} else if !errors.Is(err, aitasks.ErrNothingToDo) {
					errs = append(errs, fmt.Errorf("triage: %w", err))
				}
			}
			if len(diff) > 0 {
				title := fmt.Sprintf("AI review of new commits %s..%s", short(oldHead), short(after.Head))
				id, err := runner.Run(ctx, "review", aitasks.Input{Repo: repo, Diff: diff, Title: title})
				if err == nil {
					last = id
				} else if !errors.Is(err, aitasks.ErrNothingToDo) {
					errs = append(errs, fmt.Errorf("review: %w", err))
				}
			}
			return last, errors.Join(errs...)
		}})
		if err != nil {
			s.logger.Warn("auto-review not started", "repo", repo, "error", err)
		}
	}
}

func short(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
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
