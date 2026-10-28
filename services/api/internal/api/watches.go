package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/forge"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/model"
)

// Organisation watches (accounts mode): a member asks for the public
// repositories an organisation or user on a git host creates later to be
// added to their CodeSynapse organisation, so the import is not repeated.
// A watch runs as the member who created it, rechecked on every scan, and
// adds only public repositories, anonymously, the way POST /repos does.

const (
	// watchScanInterval is how often a watch lists its organisation again.
	// Unauthenticated provider APIs allow few requests, so keep it gentle.
	watchScanInterval = 15 * time.Minute
	// watchRetry keeps a failing watch from being retried every tick.
	watchRetry = time.Hour
)

type watchAddKey struct{}

// signedInOrWatching says whether p may attach a repository: a signed-in
// session, or the delegated identity of a watch scan, which only the scan
// (watchContext) can mark.
func signedInOrWatching(ctx context.Context, p accounts.Principal) bool {
	if p.Via == "session" {
		return true
	}
	watching, _ := ctx.Value(watchAddKey{}).(bool)
	return p.Via == "automation" && watching
}

// WatchView is one watch as the dashboard shows it.
type WatchView struct {
	Key             string `json:"key"`
	Provider        string `json:"provider"`
	Host            string `json:"host"`
	Org             string `json:"org"`
	IncludeArchived bool   `json:"include_archived"`
	IncludeForks    bool   `json:"include_forks"`
	Paused          bool   `json:"paused"`
	LastScan        string `json:"last_scan,omitempty"`
	LastStatus      string `json:"last_status,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	NextScan        string `json:"next_scan,omitempty"`
	Added           int    `json:"added"`
	// Mine says the signed-in member created it and can change it; admins can too.
	Mine bool `json:"mine"`
}

func watchView(w accounts.OrgWatch, userID string) WatchView {
	v := WatchView{Key: w.Key(), Provider: w.Provider, Host: w.Host, Org: w.Login, IncludeArchived: w.IncludeArchived, IncludeForks: w.IncludeForks,
		Paused: w.Paused, LastStatus: w.LastStatus, LastError: w.LastError, Added: w.Added, Mine: w.ActorID == userID}
	if !w.LastScan.IsZero() {
		v.LastScan = w.LastScan.Format(time.RFC3339)
	}
	if !w.Paused && !w.NextScan.IsZero() {
		v.NextScan = w.NextScan.Format(time.RFC3339)
	}
	return v
}

func (s *Server) watchRoutes() {
	s.route("GET /api/v1/watches", orgRole(accounts.Viewer), s.handleListWatches)
	s.route("POST /api/v1/watches", orgRole(accounts.Member), s.handleAddWatch)
	s.route("PUT /api/v1/watches/{host}/{login}", orgRole(accounts.Member), s.handleUpdateWatch)
	s.route("DELETE /api/v1/watches/{host}/{login}", orgRole(accounts.Member), s.handleDeleteWatch)
}

// watchCaller returns the signed-in member and the organisation acted in.
func (s *Server) watchCaller(w http.ResponseWriter, r *http.Request, mutating bool) (*http.Request, accounts.Principal, accounts.Membership, bool) {
	p, ok := s.caller(w, r)
	if !ok {
		return r, p, accounts.Membership{}, false
	}
	if mutating && p.Via != "session" {
		// Like adding a repository: a person decides, an API key does not.
		s.accountError(w, accounts.ErrForbidden)
		return r, p, accounts.Membership{}, false
	}
	m, err := scopeFrom(r.Context()).selected()
	if err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return r, p, m, false
	}
	if mutating {
		var ok bool
		if r, ok = s.executionIn(w, r, p, m.Org.ID); !ok {
			return r, p, m, false
		}
	}
	return r, p, m, true
}

func (s *Server) handleListWatches(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	m, err := scopeFrom(r.Context()).selected()
	if err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	list, err := s.cfg.Accounts.St.OrgWatches(r.Context(), m.Org.ID)
	if err != nil {
		s.accountError(w, err)
		return
	}
	out := make([]WatchView, 0, len(list))
	for _, x := range list {
		s.renewWatch(r.Context(), p, m, &x)
		out = append(out, watchView(x, p.User.ID))
	}
	writeJSON(w, http.StatusOK, map[string]any{"watches": out, "max": accounts.MaxWatchesPerOrg})
}

// renewWatch gives a watch the creator's current dashboard session when they
// look at it: delegated sessions last seven days, and a watch should not stop
// because its creator did not open the page that week. Audited; if the audit
// entry cannot be written the watch keeps its old identity.
func (s *Server) renewWatch(ctx context.Context, p accounts.Principal, m accounts.Membership, x *accounts.OrgWatch) {
	if p.Via != "session" || x.ActorID != p.User.ID {
		return
	}
	id, err := s.cfg.Accounts.DashboardExecutionSession(ctx, p, m.Org.ID)
	if err != nil || id == x.Execution {
		return
	}
	if _, err := s.cfg.Accounts.Record(ctx, accounts.Event{UserID: p.User.ID, Via: p.Via, OrgID: m.Org.ID, Action: "watch.renew", Target: x.Key(), Outcome: accounts.Success, Status: http.StatusOK}); err != nil {
		return
	}
	renewed := *x
	renewed.Execution = id
	if err := s.cfg.Accounts.St.PutOrgWatch(ctx, renewed); err != nil {
		return
	}
	x.Execution = id
}

type watchRequest struct {
	Provider        string `json:"provider"`
	Host            string `json:"host"`
	Org             string `json:"org"`
	IncludeArchived bool   `json:"include_archived"`
	IncludeForks    bool   `json:"include_forks"`
}

//nolint:contextcheck // watchCaller adds the validated execution identity to the request context
func (s *Server) handleAddWatch(w http.ResponseWriter, r *http.Request) {
	r, p, m, ok := s.watchCaller(w, r, true)
	if !ok {
		return
	}
	var req watchRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	kind, host, login, err := parseOrgInput(req.Provider, req.Host, req.Org)
	if err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	existing, err := s.cfg.Accounts.St.OrgWatches(r.Context(), m.Org.ID)
	if err != nil {
		s.accountError(w, err)
		return
	}
	key := accounts.WatchKey(host, login)
	if len(existing) >= accounts.MaxWatchesPerOrg && !slices.ContainsFunc(existing, func(x accounts.OrgWatch) bool { return x.Key() == key }) {
		s.problem(w, http.StatusConflict, fmt.Errorf("an organisation can watch at most %d accounts; remove one first", accounts.MaxWatchesPerOrg))
		return
	}
	c, err := s.publicForgeClient(r.Context(), kind, host, login)
	if err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	// What exists now is the baseline: the person already chose what to add,
	// so only repositories created after this point are added.
	listed, err := c.ListRepos(ctx, login)
	if err != nil {
		if errors.Is(err, forge.ErrNotFound) {
			s.problem(w, http.StatusNotFound, errors.New(login+" was not found on "+host+", or it has no public repositories"))
			return
		}
		s.orgError(w, err)
		return
	}
	if _, err := s.cfg.Accounts.Record(r.Context(), accounts.Event{UserID: p.User.ID, Via: p.Via, OrgID: m.Org.ID, Action: "watch.create", Target: key, Outcome: accounts.Success, Status: http.StatusCreated}); err != nil {
		s.accountError(w, err)
		return
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	watch := accounts.OrgWatch{OrgID: m.Org.ID, ActorID: p.User.ID, Execution: identityOf(r.Context()), Provider: string(kind), Host: host, Login: login,
		IncludeArchived: req.IncludeArchived, IncludeForks: req.IncludeForks, NextScan: now.Add(watchScanInterval)}
	if err := s.cfg.Accounts.St.PutOrgWatch(ctx, watch); err != nil {
		s.accountError(w, err)
		return
	}
	known := make([]string, 0, min(len(listed), accounts.MaxWatchKnown))
	for _, x := range listed[:min(len(listed), accounts.MaxWatchKnown)] {
		known = append(known, x.Name)
	}
	if err := s.cfg.Accounts.St.RecordWatchScan(ctx, m.Org.ID, key, accounts.WatchScan{LastScan: now, LastStatus: "watching", NextScan: now.Add(watchScanInterval), Known: known}); err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, watchView(watch, p.User.ID))
}

type watchUpdate struct {
	Paused          *bool `json:"paused"`
	IncludeArchived *bool `json:"include_archived"`
	IncludeForks    *bool `json:"include_forks"`
}

// ownWatch loads the watch named in the path, which the caller may change
// when they created it or administer the organisation.
func (s *Server) ownWatch(w http.ResponseWriter, r *http.Request, p accounts.Principal, m accounts.Membership) (accounts.OrgWatch, bool) {
	key := accounts.WatchKey(r.PathValue("host"), r.PathValue("login"))
	list, err := s.cfg.Accounts.St.OrgWatches(r.Context(), m.Org.ID)
	if err != nil {
		s.accountError(w, err)
		return accounts.OrgWatch{}, false
	}
	i := slices.IndexFunc(list, func(x accounts.OrgWatch) bool { return x.Key() == key })
	if i < 0 {
		s.problem(w, http.StatusNotFound, errors.New("no such watch"))
		return accounts.OrgWatch{}, false
	}
	if list[i].ActorID != p.User.ID && !m.Role.AtLeast(accounts.Admin) {
		s.accountError(w, accounts.ErrForbidden)
		return accounts.OrgWatch{}, false
	}
	return list[i], true
}

//nolint:contextcheck // watchCaller adds the validated execution identity to the request context
func (s *Server) handleUpdateWatch(w http.ResponseWriter, r *http.Request) {
	r, p, m, ok := s.watchCaller(w, r, true)
	if !ok {
		return
	}
	watch, ok := s.ownWatch(w, r, p, m)
	if !ok {
		return
	}
	var req watchUpdate
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	if req.Paused != nil {
		watch.Paused = *req.Paused
	}
	if req.IncludeArchived != nil {
		watch.IncludeArchived = *req.IncludeArchived
	}
	if req.IncludeForks != nil {
		watch.IncludeForks = *req.IncludeForks
	}
	// Whoever changes a watch runs it from now on.
	watch.ActorID, watch.Execution = p.User.ID, identityOf(r.Context())
	if _, err := s.cfg.Accounts.Record(r.Context(), accounts.Event{UserID: p.User.ID, Via: p.Via, OrgID: m.Org.ID, Action: "watch.update", Target: watch.Key(), Outcome: accounts.Success, Status: http.StatusOK}); err != nil {
		s.accountError(w, err)
		return
	}
	if err := s.cfg.Accounts.St.PutOrgWatch(r.Context(), watch); err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, watchView(watch, p.User.ID))
}

//nolint:contextcheck // watchCaller adds the validated execution identity to the request context
func (s *Server) handleDeleteWatch(w http.ResponseWriter, r *http.Request) {
	r, p, m, ok := s.watchCaller(w, r, true)
	if !ok {
		return
	}
	watch, ok := s.ownWatch(w, r, p, m)
	if !ok {
		return
	}
	if _, err := s.cfg.Accounts.Record(r.Context(), accounts.Event{UserID: p.User.ID, Via: p.Via, OrgID: m.Org.ID, Action: "watch.delete", Target: watch.Key(), Outcome: accounts.Success, Status: http.StatusNoContent}); err != nil {
		s.accountError(w, err)
		return
	}
	if err := s.cfg.Accounts.St.DeleteOrgWatch(r.Context(), m.Org.ID, watch.Key()); err != nil {
		s.accountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- scanning ---------------------------------------------------------------

// scanWatches lists the watches that are due and scans them one at a time in
// the background; a tick that finds a scan still running does nothing.
func (s *Server) scanWatches(ctx context.Context, now time.Time, logger *slog.Logger) {
	if s.cfg.Accounts == nil || !s.watching.CompareAndSwap(false, true) {
		return
	}
	due, err := s.cfg.Accounts.St.DueOrgWatches(ctx, now)
	if err != nil || len(due) == 0 {
		s.watching.Store(false)
		if err != nil {
			logger.Warn("listing due organisation watches", "error", err)
		}
		return
	}
	go func() {
		defer s.watching.Store(false)
		for _, w := range due {
			if ctx.Err() != nil {
				return
			}
			s.scanWatch(ctx, w, time.Now().UTC(), logger)
		}
	}()
}

// watchContext rebuilds what the creating member could do: the same
// execution session, a current membership of at least Member, and the
// marker that lets this scan attach repositories.
func (s *Server) watchContext(ctx context.Context, w accounts.OrgWatch) (context.Context, error) {
	ctx, err := execution.With(ctx, w.Execution)
	if err != nil {
		return nil, err
	}
	if w.OrgID == "" || w.ActorID == "" || w.Execution.SubjectID != w.ActorID || w.Execution.TenantID != w.OrgID {
		return nil, execution.ErrConflict
	}
	if _, err := s.cfg.Accounts.ResolveExecutionSession(ctx, w.ActorID, w.OrgID, w.Execution.SessionID); err != nil {
		return nil, err
	}
	p := accounts.Principal{User: accounts.User{ID: w.ActorID}, OrgID: w.OrgID, Via: "automation"}
	if err := s.cfg.Accounts.ValidatePrincipal(ctx, p); err != nil {
		return nil, err
	}
	if err := s.cfg.Accounts.Require(ctx, p, w.OrgID, accounts.Member); err != nil {
		return nil, err
	}
	members, err := s.cfg.Accounts.St.UserMemberships(ctx, w.ActorID)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(members, func(m accounts.Membership) bool { return m.Org.ID == w.OrgID })
	if i < 0 {
		return nil, accounts.ErrForbidden
	}
	ctx = context.WithValue(ctx, principalKey{}, p)
	ctx = context.WithValue(ctx, scopeKey{}, scope{graphs: map[string]accounts.Role{}, reach: map[string]accounts.Reach{}, orgs: []accounts.Membership{members[i]}})
	return context.WithValue(ctx, watchAddKey{}, true), nil
}

func (s *Server) scanWatch(ctx context.Context, w accounts.OrgWatch, now time.Time, logger *slog.Logger) {
	fail := func(status string, err error) {
		logger.Warn("organisation watch not scanned", "org", w.OrgID, "watch", w.Key(), "error", err)
		msg := err.Error()
		if len(msg) > 300 {
			msg = msg[:300]
		}
		if err := s.cfg.Accounts.St.RecordWatchScan(ctx, w.OrgID, w.Key(), accounts.WatchScan{LastScan: now, LastStatus: status, LastError: msg, NextScan: now.Add(watchRetry)}); err != nil {
			logger.Warn("recording organisation watch scan", "error", err)
		}
	}
	wctx, err := s.watchContext(ctx, w)
	if err != nil {
		status := "paused"
		if errors.Is(err, accounts.ErrExecutionSessionClosed) {
			status = "needs sign-in" // renewed when its creator opens Repositories
		}
		fail(status, err)
		return
	}
	c, err := s.publicForgeClient(wctx, forge.Kind(w.Provider), w.Host, w.Login)
	if err != nil {
		fail("failed", err)
		return
	}
	lctx, cancel := context.WithTimeout(wctx, 90*time.Second)
	defer cancel()
	listed, err := c.ListRepos(lctx, w.Login)
	if err != nil {
		fail("failed", err)
		return
	}
	have, err := s.cfg.Accounts.St.OrgRepositories(wctx, w.OrgID)
	if err != nil {
		fail("failed", err)
		return
	}
	// A member's own entry is not the organisation's: an organisation
	// watch takes it over (spaceTakeover), any other watch skips it.
	linked := map[string]bool{}
	for _, x := range have {
		linked[x.Graph] = !x.Personal()
	}
	known := map[string]bool{}
	for _, k := range w.Known {
		known[k] = true
	}
	added, lastErr := 0, ""
	for _, x := range listed {
		if known[x.Name] || !keepListed(x, w.IncludeArchived, w.IncludeForks) {
			continue
		}
		name := forge.RepoName(w.Host, w.Login, x.Name)
		if !linked[name] {
			if added == maxClonesPerScan {
				break // the rest wait for the next scan
			}
			if ok, err := s.addWatched(wctx, w, c.CloneURL(w.Login, x.Name), name); err != nil {
				lastErr = name + ": " + err.Error()
			} else if ok {
				added++
			}
		}
		known[x.Name] = true
	}
	seen := make([]string, 0, min(len(known), accounts.MaxWatchKnown))
	for k := range known {
		seen = append(seen, k)
	}
	slices.Sort(seen)
	seen = seen[:min(len(seen), accounts.MaxWatchKnown)]
	status := "ok"
	if lastErr != "" {
		status = "some failed"
	}
	if len(lastErr) > 300 {
		lastErr = lastErr[:300]
	}
	if err := s.cfg.Accounts.St.RecordWatchScan(ctx, w.OrgID, w.Key(), accounts.WatchScan{LastScan: now, LastStatus: status, LastError: lastErr, NextScan: now.Add(watchScanInterval), Added: added, Known: seen}); err != nil {
		logger.Warn("recording organisation watch scan", "error", err)
	}
}

// watchSkip says the watch has nothing to add: the organisation already has
// the repository in another space it does not take over (another member's
// for a member's watch, the organisation's, or a member's private entry),
// or the creator is a member, whose space it would go to, and members' own
// repositories are turned off. An organisation watch takes a member's
// public entry over instead.
func watchSkip(err error) bool {
	var taken errSpaceTaken
	return errors.As(err, &taken) || errors.Is(err, errMemberReposOff)
}

// watchSkips reports, before anything is audited, whether linkRepo would
// answer watchSkip for this add, so a no-op leaves no watch.repo_add entry;
// other skips in a scan (known, filtered or linked repositories) are not
// audited either. linkPolicyRepository decides again under the policy lock.
func (s *Server) watchSkips(ctx context.Context, name, remote string) (bool, error) {
	entry, _, err := s.linkEntry(ctx, name, remote, accounts.Public, "")
	if err != nil {
		return false, err
	}
	if entry.Personal() {
		on, err := s.cfg.Accounts.St.MemberReposEnabled(ctx, entry.OrgID)
		if err != nil {
			return false, err
		}
		if !on {
			return true, nil
		}
	}
	existing, err := s.cfg.Accounts.St.GraphRepositories(ctx, name)
	if err != nil {
		return false, err
	}
	_, err = spaceTakeover(existing, entry)
	return watchSkip(err), nil
}

// addWatched attaches one new public repository the way POST /repos does:
// an indexed graph is linked only for the same remote, otherwise it is
// cloned anonymously. The audit entry is written first; without it nothing
// is added. It reports whether anything was added: a skip (watchSkips) is
// not, and is not audited.
func (s *Server) addWatched(ctx context.Context, w accounts.OrgWatch, remote, name string) (bool, error) {
	if !validRepoName(name) {
		return false, errors.New("the name is not valid here")
	}
	remote, err := gitx.ValidateRemote(ctx, remote, gitx.RemoteOptions{AllowPrivate: s.cfg.AllowPrivateGit})
	if err != nil {
		return false, err
	}
	if !publicRemote(ctx, remote) {
		return false, errors.New("not readable without a token")
	}
	if skip, err := s.watchSkips(ctx, name, remote); err != nil || skip {
		return false, err
	}
	if _, err := s.cfg.Accounts.Record(ctx, accounts.Event{UserID: w.ActorID, Via: "automation", OrgID: w.OrgID, Action: "watch.repo_add", Target: name, Outcome: accounts.Success}); err != nil {
		return false, err
	}
	if n, err := s.st.Node(ctx, model.NodeID(name, model.KindRepository, name)); err == nil {
		if n.URL == "" || !sameRemote(n.URL, remote) {
			return false, errors.New("the name is already used for another source")
		}
		err := s.linkRepo(ctx, name, remote, accounts.Public, "")
		if watchSkip(err) {
			return false, nil
		}
		return err == nil, err
	}
	if err := s.linkRepo(ctx, name, remote, accounts.Public, ""); watchSkip(err) {
		return false, nil // nothing to clone
	} else if err != nil {
		return false, err
	}
	org, authorize := s.jobAuthorization(ctx, name, accounts.Admin)
	d, err := s.durableIf(ctx, "clone", accounts.Admin, cloneParams{Remote: remote, Name: name})
	if err != nil {
		return false, err
	}
	_, err = s.startCloneAuth(remote, name, "", gitx.Auth{}, JobOptions{OrgID: org, Identity: identityOf(ctx), Authorize: authorize, Durable: d}) //nolint:contextcheck // jobs outlive the scan
	return err == nil, err
}
