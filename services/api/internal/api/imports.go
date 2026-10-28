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
// Listing uses the selected organisation's own Git token for the host when
// it has one, so private repositories the token reads are listed with the
// token's permission on each; without a token the listing is anonymous and
// shows public repositories only. The token goes to that host alone (forge
// follows no redirects), and adding a private repository still proves access
// with it (proveAccess) before anything is cloned.

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
	Private       bool   `json:"private,omitempty"`
	// Permission is the organisation token's access: admin, write or read.
	Permission string `json:"permission,omitempty"`
	PushedAt   string `json:"pushed_at,omitempty"`
	// Added says the selected organisation already has this repository.
	Added bool `json:"added"`
}

// publicForgeClient is the anonymous client for a login on host. Hosts other
// than github.com come from the user, so they pass the same SSRF check as
// clone URLs.
func (s *Server) publicForgeClient(ctx context.Context, kind forge.Kind, host, login string) (forge.Client, error) {
	c := forge.Client{Kind: kind, Host: host, BaseURL: s.cfg.ForgeAPI}
	if s.cfg.ForgeAPI == "" && host != forge.DefaultHost {
		if _, err := gitx.ValidateRemote(ctx, c.CloneURL(login, "x"), gitx.RemoteOptions{AllowPrivate: s.cfg.AllowPrivateGit}); err != nil {
			return c, err
		}
	}
	return c, nil
}

// keepListed says whether a listed repository can be imported under the
// archived and fork filters: public (or private when the listing used the
// organisation's token), with a name that is safe to clone.
func keepListed(x forge.Repo, archived, forks, private bool) bool {
	return validRepoSegment(x.Name) && (private || !x.Private) && (archived || !x.Archived) && (forks || !x.Fork)
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
	c, err := s.publicForgeClient(r.Context(), kind, host, login)
	if err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	tok, err := s.cfg.Accounts.OpenGitToken(r.Context(), m.Org.ID, host)
	switch {
	case err == nil:
		c.Token = tok
	case !errors.Is(err, accounts.ErrNotFound) && !errors.Is(err, accounts.ErrNoSecretKey):
		s.accountError(w, err)
		return
	}
	authed := c.Token != ""
	if authed {
		if _, err := s.cfg.Accounts.Record(r.Context(), accounts.Event{UserID: p.User.ID, Via: p.Via, OrgID: m.Org.ID,
			Action: "git_token.list", Target: host + "/" + login, Outcome: accounts.Success, Status: http.StatusOK}); err != nil {
			s.accountError(w, err)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	listing, err := c.ListReposDetail(ctx, login)
	if err != nil {
		if errors.Is(err, forge.ErrNotFound) {
			what := "public repositories"
			if authed {
				what = "repositories the organisation's token can read"
			}
			s.problem(w, http.StatusNotFound, errors.New(login+" was not found on "+host+", or it has no "+what))
			return
		}
		s.orgError(w, err)
		return
	}
	listed := listing.Repos
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
		if !keepListed(x, req.IncludeArchived, req.IncludeForks, authed) {
			continue
		}
		name := forge.RepoName(host, login, x.Name)
		out = append(out, ImportCandidate{Name: x.Name, Repo: name, URL: c.CloneURL(login, x.Name), DefaultBranch: x.DefaultBranch,
			Description: firstLineOf(x.Description), Archived: x.Archived, Fork: x.Fork, Private: x.Private, Permission: x.Permission,
			PushedAt: x.PushedAt, Added: added[name]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": kind, "host": host, "org": login, "repos": out, "truncated": len(out) == maxImportListed,
		"authenticated": authed, "sso_restricted": listing.SSORestricted})
}
