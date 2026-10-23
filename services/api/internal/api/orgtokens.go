package api

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/gitx"
)

// Organisation Git tokens and private repositories (SaaS phase 2c).
//
// An organisation reads a private repository only with its own token: it
// proves access when adding the repository (one ls-remote), the clone uses
// its token, and every sync checks each organisation's token again. An
// organisation whose token stops reading the remote loses the graph
// (accounts.AccessLost, invisible in its scope) until a working token
// restores it. Graphs are shared only between organisations that each
// proved access this way.

const accessTimeout = 20 * time.Second

// canRead is gitx.CanRead; tests replace it to simulate hosts.
var canRead = gitx.CanRead

type gitTokenRequest struct {
	Token string `json:"token"`
}

func (s *Server) handleGitTokens(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.Member)
	if !ok {
		return
	}
	ts, err := s.cfg.Accounts.GitTokens(r.Context(), p, o.ID)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"org": o.Slug, "tokens": ts, "enabled": s.cfg.Accounts.Secrets != nil})
}

func (s *Server) handleSetGitToken(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.Admin)
	if !ok {
		return
	}
	var req gitTokenRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	host, err := accounts.NormalizeHost(r.PathValue("host"))
	if err != nil {
		s.accountError(w, err)
		return
	}
	// The same rules as remotes: https, no internal hosts unless allowed.
	if _, err := gitx.ValidateRemote(r.Context(), "https://"+host+"/", gitx.RemoteOptions{AllowPrivate: s.cfg.AllowPrivateGit}); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	t, err := s.cfg.Accounts.SetGitToken(r.Context(), p, o.ID, host, req.Token)
	if err != nil {
		s.accountError(w, err)
		return
	}
	restored := s.reverifyOrgHost(r.Context(), o.ID, host)
	writeJSON(w, http.StatusOK, map[string]any{"token": t, "restored": restored})
}

func (s *Server) handleRemoveGitToken(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.Admin)
	if !ok {
		return
	}
	if err := s.cfg.Accounts.RemoveGitToken(r.Context(), p, o.ID, r.PathValue("host")); err != nil {
		s.accountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func hostOf(remote string) string {
	u, err := url.Parse(remote)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// orgAuth opens orgID's token for remote's host (accounts.ErrNotFound
// without one).
func (s *Server) orgAuth(ctx context.Context, orgID, remote string) (gitx.Auth, error) {
	host := hostOf(remote)
	tok, err := s.cfg.Accounts.OpenGitToken(ctx, orgID, host)
	if err != nil {
		return gitx.Auth{}, err
	}
	return gitx.Auth{Host: host, Token: tok}, nil
}

// proveAccess checks that orgID's own token reads remote.
func (s *Server) proveAccess(ctx context.Context, orgID, remote string) (gitx.Auth, error) {
	auth, err := s.orgAuth(ctx, orgID, remote)
	if err != nil {
		return gitx.Auth{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, accessTimeout)
	defer cancel()
	return auth, canRead(ctx, remote, auth)
}

// verifyAccess re-checks every organisation that uses graph as a private
// repository and records the outcome. Refused or missing tokens lose
// access; network failures change nothing.
func (s *Server) verifyAccess(ctx context.Context, graph string) {
	if s.cfg.Accounts == nil {
		return
	}
	links, err := s.cfg.Accounts.St.GraphRepositories(ctx, graph)
	if err != nil {
		s.logger.Warn("listing repository links failed", "repo", graph, "error", err)
		return
	}
	for _, l := range links {
		if l.Visibility == accounts.Private {
			s.verifyLink(ctx, l)
		}
	}
}

// verifyLink checks one private entry and reports whether it is usable.
func (s *Server) verifyLink(ctx context.Context, l accounts.Repository) bool {
	_, err := s.proveAccess(ctx, l.OrgID, l.Remote)
	access := accounts.AccessOK
	switch {
	case err == nil:
	case errors.Is(err, gitx.ErrNoAccess), errors.Is(err, accounts.ErrNotFound):
		access = accounts.AccessLost
	default:
		s.logger.Warn("checking repository access failed; keeping the last result", "repo", l.Graph, "org", l.OrgID, "error", err)
		return l.Usable()
	}
	if err := s.cfg.Accounts.St.SetRepositoryAccess(ctx, l.OrgID, l.Graph, access, time.Now().UTC().Truncate(time.Microsecond)); err != nil {
		s.logger.Warn("recording repository access failed", "repo", l.Graph, "org", l.OrgID, "error", err)
	}
	if previous := cmp.Or(l.Access, accounts.AccessOK); access != previous {
		action := "repo.access_restored"
		if access == accounts.AccessLost {
			action = "repo.access_lost"
		}
		s.record(ctx, accounts.Event{Action: action, Via: "system", OrgID: l.OrgID, Target: l.Graph, Outcome: "success"})
	}
	return access == accounts.AccessOK
}

// reverifyOrgHost re-checks orgID's private repositories on host after its
// token changed, and returns the graphs that became usable again.
func (s *Server) reverifyOrgHost(ctx context.Context, orgID, host string) []string {
	repos, err := s.cfg.Accounts.St.OrgRepositories(ctx, orgID)
	if err != nil {
		return nil
	}
	restored := []string{}
	for _, l := range repos {
		if l.Visibility == accounts.Private && hostOf(l.Remote) == host && !l.Usable() && s.verifyLink(ctx, l) {
			restored = append(restored, l.Graph)
		}
	}
	return restored
}

// pushAuth picks credentials to push a fix branch: an organisation pushes
// with its own token. Single-tenant installs use the installation token.
func (s *Server) pushAuth(r *http.Request, graph, remote string) (gitx.Auth, error) {
	if s.cfg.Accounts == nil {
		return s.authFor(remote), nil
	}
	org, err := s.repoOrg(r, graph)
	if err != nil {
		return gitx.Auth{}, err
	}
	auth, err := s.orgAuth(r.Context(), org, remote)
	if err != nil {
		return gitx.Auth{}, errors.New("pushing a fix branch needs your organisation's Git token for " + hostOf(remote))
	}
	return auth, nil
}

// pushAuthFor is pushAuth for a context without a request (workers): the
// organisation comes from the one repository in scope.
func (s *Server) pushAuthFor(ctx context.Context, graph, remote string) (gitx.Auth, error) {
	if s.cfg.Accounts == nil {
		return s.authFor(remote), nil
	}
	reach, ok := scopeFrom(ctx).reach[graph]
	if !ok || reach.Ambiguous || reach.OrgID == "" {
		return gitx.Auth{}, accounts.ErrNotFound
	}
	auth, err := s.orgAuth(ctx, reach.OrgID, remote)
	if err != nil {
		return gitx.Auth{}, errors.New("pushing a fix branch needs your organisation's Git token for " + hostOf(remote))
	}
	return auth, nil
}

// syncAuth uses only the selected tenant's credentials in accounts mode.
func (s *Server) syncAuth(ctx context.Context, graph, remote string) gitx.Auth {
	if s.cfg.Accounts == nil {
		return s.authFor(remote)
	}
	reach, ok := scopeFrom(ctx).reach[graph]
	if !ok || reach.Ambiguous {
		return gitx.Auth{}
	}
	if auth, err := s.orgAuth(ctx, reach.OrgID, remote); err == nil {
		return auth
	}
	return gitx.Auth{} // public repositories need none
}
