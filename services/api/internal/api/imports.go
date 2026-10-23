package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/forge"
	"github.com/astrona-io/codesynapse/internal/gitx"
)

// Organisation import with accounts: the dashboard lists a provider
// organisation's repositories here, then adds the ones the person picks
// through POST /api/v1/repos, so every addition gets the usual checks
// (public or proved private access, same-remote linking, the initial admin
// grant). The installation-wide watch list (orgs.go) stays single-tenant.
//
// Listing is anonymous: organisation Git tokens are opened only to clone,
// fetch or push, so the list shows public repositories. Private ones are
// added by URL.

// maxImportListed bounds one answer; forge already caps the pages it reads.
const maxImportListed = 2000

type importListRequest struct {
	Provider        string `json:"provider"`
	Host            string `json:"host"`
	Org             string `json:"org"`
	IncludeArchived bool   `json:"include_archived"`
	IncludeForks    bool   `json:"include_forks"`
}

// ImportCandidate is one listed repository. URL is built from the validated
// host, login and name, never taken from the provider's answer.
type ImportCandidate struct {
	Name          string `json:"name"`
	Repo          string `json:"repo"`
	URL           string `json:"url"`
	DefaultBranch string `json:"default_branch,omitempty"`
	Description   string `json:"description,omitempty"`
	Archived      bool   `json:"archived,omitempty"`
	Fork          bool   `json:"fork,omitempty"`
	PushedAt      string `json:"pushed_at,omitempty"`
	// Added says the selected organisation already has this repository.
	Added bool `json:"added"`
}

func (s *Server) importRoutes() {
	s.route("POST /api/v1/imports/list", orgRole(accounts.Member), s.handleImportList)
}

func (s *Server) handleImportList(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	if p.Via != "session" {
		// Adding repositories needs a signed-in session; so does finding them.
		s.accountError(w, accounts.ErrForbidden)
		return
	}
	m, err := scopeFrom(r.Context()).selected()
	if err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	var req importListRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	kind, host, login, err := parseOrgInput(req.Provider, req.Host, req.Org)
	if err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	c := forge.Client{Kind: kind, Host: host, BaseURL: s.cfg.ForgeAPI}
	if s.cfg.ForgeAPI == "" && host != forge.DefaultHost {
		if _, err := gitx.ValidateRemote(r.Context(), c.CloneURL(login, "x"), gitx.RemoteOptions{AllowPrivate: s.cfg.AllowPrivateGit}); err != nil {
			s.problem(w, http.StatusBadRequest, err)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	listed, err := c.ListRepos(ctx, login)
	if err != nil {
		if errors.Is(err, forge.ErrNotFound) {
			s.problem(w, http.StatusNotFound, errors.New(login+" was not found on "+host+", or it has no public repositories"))
			return
		}
		s.orgError(w, err)
		return
	}
	added := map[string]bool{}
	repos, err := s.cfg.Accounts.St.OrgRepositories(r.Context(), m.Org.ID)
	if err != nil {
		s.accountError(w, err)
		return
	}
	for _, x := range repos {
		added[x.Graph] = true
	}
	out := make([]ImportCandidate, 0, min(len(listed), maxImportListed))
	for _, x := range listed {
		if len(out) == maxImportListed {
			break
		}
		if !validRepoSegment(x.Name) || x.Private || (x.Archived && !req.IncludeArchived) || (x.Fork && !req.IncludeForks) {
			continue
		}
		name := forge.RepoName(host, login, x.Name)
		out = append(out, ImportCandidate{Name: x.Name, Repo: name, URL: c.CloneURL(login, x.Name), DefaultBranch: x.DefaultBranch,
			Description: firstLineOf(x.Description), Archived: x.Archived, Fork: x.Fork, PushedAt: x.PushedAt, Added: added[name]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": kind, "host": host, "org": login, "repos": out, "truncated": len(out) == maxImportListed})
}
