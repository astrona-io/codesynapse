package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
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

// repoAuth picks the one credential an entry may use: a personal entry
// (ownerID set) only its owner's own token, an organisation entry only the
// organisation's. There is no fallback between the two, so a member cannot
// pull what the organisation's token reads into a space its admins do not
// see.
func (s *Server) repoAuth(ctx context.Context, orgID, ownerID, remote string) (gitx.Auth, error) {
	host := hostOf(remote)
	var (
		tok string
		err error
	)
	if ownerID != "" {
		tok, err = s.cfg.Accounts.OpenUserGitToken(ctx, orgID, ownerID, host)
	} else {
		tok, err = s.cfg.Accounts.OpenGitToken(ctx, orgID, host)
	}
	if err != nil {
		return gitx.Auth{}, err
	}
	return gitx.Auth{Host: host, Token: tok}, nil
}

// entryAuth is repoAuth for orgID's entry of graph (accounts.ErrNotFound
// when orgID does not have it).
func (s *Server) entryAuth(ctx context.Context, orgID, graph, remote string) (gitx.Auth, error) {
	e, err := s.orgEntry(ctx, orgID, graph)
	if err != nil {
		return gitx.Auth{}, err
	}
	return s.repoAuth(ctx, orgID, e.OwnerID, remote)
}

func (s *Server) orgEntry(ctx context.Context, orgID, graph string) (accounts.Repository, error) {
	entries, err := s.cfg.Accounts.St.GraphRepositories(ctx, graph)
	if err != nil {
		return accounts.Repository{}, err
	}
	for _, e := range entries {
		if e.OrgID == orgID {
			return e, nil
		}
	}
	return accounts.Repository{}, fmt.Errorf("repository: %w", accounts.ErrNotFound)
}

// proveAccess checks that the entry's own token (ownerID's for a personal
// entry, orgID's otherwise) reads remote.
func (s *Server) proveAccess(ctx context.Context, orgID, ownerID, remote string) (gitx.Auth, error) {
	auth, err := s.repoAuth(ctx, orgID, ownerID, remote)
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
	_, err := s.proveAccess(ctx, l.OrgID, l.OwnerID, l.Remote)
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
// token changed, and returns the graphs that became usable again. Personal
// entries are left alone: the organisation's token is not theirs.
func (s *Server) reverifyOrgHost(ctx context.Context, orgID, host string) []string {
	repos, err := s.cfg.Accounts.St.OrgRepositories(ctx, orgID)
	if err != nil {
		return nil
	}
	restored := []string{}
	for _, l := range repos {
		if !l.Personal() && l.Visibility == accounts.Private && hostOf(l.Remote) == host && !l.Usable() && s.verifyLink(ctx, l) {
			restored = append(restored, l.Graph)
		}
	}
	return restored
}

// reverifyUserHost re-checks userID's own private repositories on host, in
// every organisation they belong to, after their token changed.
func (s *Server) reverifyUserHost(ctx context.Context, userID, host string) []string {
	ms, err := s.cfg.Accounts.St.UserMemberships(ctx, userID)
	if err != nil {
		return nil
	}
	restored := []string{}
	for _, m := range ms {
		repos, err := s.cfg.Accounts.St.OrgRepositories(ctx, m.Org.ID)
		if err != nil {
			continue
		}
		for _, l := range repos {
			if l.OwnerID == userID && l.Visibility == accounts.Private && hostOf(l.Remote) == host && !l.Usable() && s.verifyLink(ctx, l) {
				restored = append(restored, l.Graph)
			}
		}
	}
	return restored
}

// pushAuth picks credentials to push a fix branch: the entry's own token
// (the owner's for a personal repository, the organisation's otherwise).
// Single-tenant installs use the installation token.
func (s *Server) pushAuth(r *http.Request, graph, remote string) (gitx.Auth, error) {
	if s.cfg.Accounts == nil {
		return s.installPushAuth(remote), nil
	}
	org, err := s.repoOrg(r, graph)
	if err != nil {
		return gitx.Auth{}, err
	}
	return s.entryPushAuth(r.Context(), org, graph, remote)
}

func (s *Server) entryPushAuth(ctx context.Context, orgID, graph, remote string) (gitx.Auth, error) {
	e, err := s.orgEntry(ctx, orgID, graph)
	if err != nil {
		return gitx.Auth{}, err
	}
	auth, err := s.repoAuth(ctx, orgID, e.OwnerID, remote)
	switch {
	case err == nil:
		return auth, nil
	case e.Personal():
		return gitx.Auth{}, errors.New("pushing a fix branch to your own repository needs your Git token for " + hostOf(remote) + " (Profile, Git tokens)")
	default:
		return gitx.Auth{}, errors.New("pushing a fix branch needs your organisation's Git token for " + hostOf(remote))
	}
}

// pushAuthFor is pushAuth for a context without a request (workers): the
// organisation comes from the one repository in scope.
func (s *Server) pushAuthFor(ctx context.Context, graph, remote string) (gitx.Auth, error) {
	if s.cfg.Accounts == nil {
		return s.installPushAuth(remote), nil
	}
	reach, ok := scopeFrom(ctx).reach[graph]
	if !ok || reach.Ambiguous || reach.OrgID == "" {
		return gitx.Auth{}, accounts.ErrNotFound
	}
	return s.entryPushAuth(ctx, reach.OrgID, graph, remote)
}

// syncAuth uses only the selected tenant entry's credentials in accounts
// mode.
func (s *Server) syncAuth(ctx context.Context, graph, remote string) gitx.Auth {
	if s.cfg.Accounts == nil {
		return s.authFor(remote)
	}
	reach, ok := scopeFrom(ctx).reach[graph]
	if !ok || reach.Ambiguous {
		return gitx.Auth{}
	}
	if auth, err := s.entryAuth(ctx, reach.OrgID, graph, remote); err == nil {
		return auth
	}
	return gitx.Auth{} // public repositories need none
}
