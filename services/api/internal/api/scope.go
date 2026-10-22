package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/accounts"
	"github.com/parisnakitakejser/codesynapse/internal/execution"
	"github.com/parisnakitakejser/codesynapse/internal/gitx"
	"github.com/parisnakitakejser/codesynapse/internal/memory"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/query"
	"github.com/parisnakitakejser/codesynapse/internal/store"
	"github.com/parisnakitakejser/codesynapse/internal/telemetry"
)

// OrgHeader selects the organisation a request acts in when the caller
// belongs to several (API keys always act in their own).
const OrgHeader = "X-CodeSynapse-Org"

// policyKind says how a route is authorised in accounts mode.
type policyKind int

const (
	// open: any authenticated caller (handlers that need more check it).
	open policyKind = iota
	// onRepo: the caller needs a role on the {repo} graph.
	onRepo
	// onReview: the caller needs a role on the review's repository.
	onReview
	// listed: any caller; the handler shows only visible repositories.
	listed
	// inOrg: the caller needs a role in the selected organisation.
	inOrg
	// platform: platform admins and the service token only.
	platform
	// local: legacy installation-wide ingestion, unavailable in accounts mode.
	local
)

type policy struct {
	kind  policyKind
	least accounts.Role
}

var (
	anyone       = policy{kind: open}
	visibleOnly  = policy{kind: listed}
	platformOnly = policy{kind: platform}
	localOnly    = policy{kind: local}
)

func repoRole(r accounts.Role) policy   { return policy{kind: onRepo, least: r} }
func reviewRole(r accounts.Role) policy { return policy{kind: onReview, least: r} }
func orgRole(r accounts.Role) policy    { return policy{kind: inOrg, least: r} }

// scope carries repository permissions and the tenant that owns private
// content. Only explicit single-tenant mode uses all.
type scope struct {
	all    bool
	graphs map[string]accounts.Role
	// reach says whose content (reviews, memory) each graph shows.
	reach map[string]accounts.Reach
	// orgs are the organisations the request acts in: the key's, the one
	// named by OrgHeader, or all of a user's.
	orgs []accounts.Membership
}

type scopeKey struct{}

func (s *Server) repoQuery(repo string) query.Service {
	return query.Service{R: store.ForRepository(s.st, repo)}
}

func scopeFrom(ctx context.Context) scope {
	sc, ok := ctx.Value(scopeKey{}).(scope)
	if !ok {
		if _, authenticated := principal(ctx); authenticated {
			return scope{}
		}
		return scope{all: true} // explicit single-tenant handlers
	}
	return sc
}

// sees reports whether the scope includes graph.
func (sc scope) sees(graph string) bool {
	if sc.all {
		return true
	}
	_, ok := sc.graphs[graph]
	return ok
}

// selected is the one organisation a creating request acts in.
func (sc scope) selected() (accounts.Membership, error) {
	if len(sc.orgs) != 1 {
		return accounts.Membership{}, errors.New("you belong to several organisations: say which one with the " + OrgHeader + " header")
	}
	return sc.orgs[0], nil
}

// membershipScope keeps account administration independent of repository policy.
func (s *Server) membershipScope(r *http.Request, p accounts.Principal) (scope, error) {
	ctx := r.Context()
	if p.Via == "service" {
		if r.Header.Get(OrgHeader) != "" {
			return scope{}, accounts.ErrNotFound
		}
		return scope{}, nil
	}
	ms, err := s.cfg.Accounts.St.UserMemberships(ctx, p.User.ID)
	if err != nil {
		return scope{}, err
	}
	want := r.Header.Get(OrgHeader)
	sc := scope{graphs: map[string]accounts.Role{}, reach: map[string]accounts.Reach{}}
	for _, m := range ms {
		if (p.OrgID != "" && m.Org.ID != p.OrgID) || (want != "" && m.Org.Slug != want) {
			continue
		}
		sc.orgs = append(sc.orgs, m)
	}
	if want != "" && len(sc.orgs) == 0 {
		return scope{}, accounts.ErrNotFound
	}
	return sc, nil
}

// scopeOf resolves repository content permissions; malformed policies deny reads.
func (s *Server) scopeOf(r *http.Request, p accounts.Principal) (scope, error) {
	sc, err := s.membershipScope(r, p)
	if err != nil || p.Via == "service" {
		return sc, err
	}
	access, err := s.cfg.Accounts.AccessOf(r.Context(), p.User.ID)
	if err != nil {
		return scope{}, err
	}
	selected := map[string]bool{}
	for i, m := range sc.orgs {
		sc.orgs[i].Role = accounts.Max(m.Role, access.OrgRole(m.Org.ID))
		selected[m.Org.ID] = true
	}
	for graph, reach := range access.Scope(selected) {
		sc.graphs[graph], sc.reach[graph] = reach.Role, reach
	}
	return sc, nil
}

// route registers h under pattern with an authorisation policy.
func (s *Server) route(pattern string, pol policy, h http.HandlerFunc) {
	s.mux.HandleFunc(pattern, s.guard(pol, h))
}

func (s *Server) guard(pol policy, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Accounts == nil {
			h(w, r)
			return
		}
		p, ok := principal(r.Context())
		if !ok {
			if publicPaths[r.Method+" "+r.URL.Path] {
				h(w, r)
			} else {
				s.accountError(w, accounts.ErrUnauthorized)
			}
			return
		}
		resolveScope := s.membershipScope
		if pol.kind == onRepo || pol.kind == onReview || pol.kind == listed {
			resolveScope = s.scopeOf
		}
		sc, err := resolveScope(r, p)
		if err != nil {
			s.accountError(w, err)
			return
		}
		ctx := context.WithValue(r.Context(), scopeKey{}, sc)
		org := ""
		if len(sc.orgs) == 1 {
			org = sc.orgs[0].Org.ID
		}
		ctx = telemetry.WithTenant(ctx, org)
		if slot, ok := ctx.Value(tenantSlot{}).(*string); ok {
			*slot = telemetry.Tenant(ctx)
		}
		r = r.WithContext(ctx)
		switch pol.kind {
		case platform:
			if !p.User.PlatformAdmin {
				s.problem(w, http.StatusForbidden, errors.New("platform admins only"))
				return
			}
		case local:
			s.problem(w, http.StatusForbidden, errors.New("use an organisation repository in accounts mode"))
			return
		case onRepo:
			if !s.allowRepo(w, sc, r.PathValue("repo"), pol.least) {
				return
			}
		case onReview:
			rv, err := s.st.GetReview(ctx, r.PathValue("id"))
			if err != nil {
				s.problem(w, http.StatusNotFound, errors.New("no such review"))
				return
			}
			if !seesReview(sc, rv.Repo, rv.OrgID) {
				s.problem(w, http.StatusNotFound, errors.New("no such review"))
				return
			}
			if !s.allowRepo(w, sc, rv.Repo, pol.least) {
				return
			}
		case inOrg:
			m, err := sc.selected()
			if err != nil {
				s.problem(w, http.StatusBadRequest, err)
				return
			}
			if err := s.cfg.Accounts.Require(ctx, p, m.Org.ID, pol.least); err != nil {
				s.problem(w, http.StatusForbidden, errors.New("your role in "+m.Org.Name+" does not allow this"))
				return
			}
		case open, listed:
		default:
			s.accountError(w, accounts.ErrForbidden)
			return
		}
		r, ok = s.executionRequest(w, r, false)
		if ok {
			h(w, r)
		}
	}
}

// allowRepo writes 404 for graphs outside the scope (their existence is not
// revealed) and 403 for a role that is too weak.
func (s *Server) allowRepo(w http.ResponseWriter, sc scope, graph string, least accounts.Role) bool {
	role, ok := sc.graphs[graph]
	if !ok {
		s.problem(w, http.StatusNotFound, errors.New("no such repository"))
		return false
	}
	if !role.AtLeast(least) {
		s.problem(w, http.StatusForbidden, errors.New("your role does not allow this"))
		return false
	}
	return true
}

// visibleRepos keeps the repository nodes the request may see.
func visibleRepos(ctx context.Context, repos []model.Node) []model.Node {
	sc := scopeFrom(ctx)
	if sc.all {
		return repos
	}
	return slices.DeleteFunc(slices.Clone(repos), func(n model.Node) bool { return !sc.sees(n.Name) })
}

// linkRepo atomically links the repository and grants its creating member ADMIN.
func (s *Server) linkRepo(r *http.Request, graph, remote, visibility string) error {
	if s.cfg.Accounts == nil {
		return nil
	}
	sc := scopeFrom(r.Context())
	m, err := sc.selected()
	if err != nil {
		return err
	}
	p, _ := principal(r.Context())
	if p.Via != "session" {
		return accounts.ErrForbidden
	}
	id, err := execution.Require(r.Context())
	if err != nil {
		return err
	}
	if _, err := s.cfg.Accounts.ResolveExecutionSession(r.Context(), p.User.ID, m.Org.ID, id.SessionID); err != nil {
		return err
	}
	err = s.cfg.Accounts.St.AddRepository(r.Context(), accounts.Repository{OrgID: m.Org.ID, Graph: graph, Remote: remote,
		Visibility: visibility, AddedBy: p.User.ID, AddedAt: time.Now().UTC().Truncate(time.Microsecond)})
	if errors.Is(err, accounts.ErrExists) {
		return nil // already linked: adding it again is fine
	}
	return err
}

// publicRemote reports whether remote can be read without credentials,
// which is what lets an organisation share an existing graph or clone it
// without any token. It probes HEAD: an empty branch (the default) used
// to fail validation and made every such repository look private.
func publicRemote(ctx context.Context, remote string) bool {
	ctx, cancel := context.WithTimeout(ctx, accessTimeout)
	defer cancel()
	return canRead(ctx, remote, gitx.Auth{}) == nil
}

type attachRequest struct {
	Graph string `json:"graph"`
}

// handleAttachRepo requires the organisation owner to prove remote access.
//
//nolint:contextcheck // bindExecutionRequest adds validated identity to the incoming request context
func (s *Server) handleAttachRepo(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.Owner)
	if !ok {
		return
	}
	if p.Via != "session" {
		s.accountError(w, accounts.ErrForbidden)
		return
	}
	session, err := executionHeader(r)
	if err != nil {
		s.problem(w, http.StatusForbidden, err)
		return
	}
	r, ok = s.bindExecutionRequest(w, r, p.User.ID, o.ID, session)
	if !ok {
		return
	}
	var req attachRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, 400, err)
		return
	}
	n, err := s.st.Node(r.Context(), model.NodeID(req.Graph, model.KindRepository, req.Graph))
	if err != nil {
		s.problem(w, http.StatusNotFound, errors.New("no indexed repository named "+req.Graph))
		return
	}
	if n.URL == "" {
		s.problem(w, http.StatusForbidden, errors.New("register a repository URL with organisation credentials"))
		return
	}
	vis := accounts.Private
	if publicRemote(r.Context(), n.URL) {
		vis = accounts.Public
	}
	if vis == accounts.Private {
		if _, err := s.proveAccess(r.Context(), o.ID, n.URL); err != nil {
			s.accountError(w, accounts.ErrForbidden)
			return
		}
	}
	if _, err := s.cfg.Accounts.ResolveExecutionSession(r.Context(), p.User.ID, o.ID, session); err != nil {
		s.accountError(w, err)
		return
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	err = s.cfg.Accounts.St.AddRepository(r.Context(), accounts.Repository{OrgID: o.ID, Graph: req.Graph, Remote: n.URL, Visibility: vis,
		AddedBy: p.User.ID, AddedAt: now})
	if err == nil && vis == accounts.Private {
		// Recheck the organisation credential before exposing private content.
		s.verifyLink(r.Context(), accounts.Repository{OrgID: o.ID, Graph: req.Graph, Remote: n.URL, Visibility: vis, Access: accounts.AccessOK})
	}
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"org": o.Slug, "graph": req.Graph, "visibility": vis})
}

// handleOrgRepos lists an organisation's repository entries.
func (s *Server) handleOrgRepos(w http.ResponseWriter, r *http.Request) {
	_, o, ok := s.orgFor(w, r, accounts.Viewer)
	if !ok {
		return
	}
	repos, err := s.cfg.Accounts.St.OrgRepositories(r.Context(), o.ID)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	p, _ := principal(r.Context())
	a, err := s.cfg.Accounts.AccessOf(r.Context(), p.User.ID)
	if err != nil {
		s.accountError(w, err)
		return
	}
	reach := a.Scope(map[string]bool{o.ID: true})
	repos = slices.DeleteFunc(repos, func(repo accounts.Repository) bool {
		r, ok := reach[repo.Graph]
		return !ok || r.OrgID != o.ID || !r.Role.AtLeast(accounts.Viewer)
	})
	writeJSON(w, 200, map[string]any{"org": o, "repositories": repos})
}

// repoOrg is the organisation a request acts in for graph: the one of the
// caller's organisations that uses it (OrgHeader picks when several do).
// Single-tenant installs use an empty organisation.
func (s *Server) repoOrg(r *http.Request, graph string) (string, error) {
	if s.cfg.Accounts == nil {
		return "", nil
	}
	sc := scopeFrom(r.Context())
	reach, ok := sc.reach[graph]
	switch {
	case !ok:
		return "", accounts.ErrNotFound
	case reach.Ambiguous:
		return "", errors.New("several of your organisations use this repository: say which one with the " + OrgHeader + " header")
	}
	return reach.OrgID, nil
}

// memNamespace is the agent-memory namespace of graph for the request:
// organisations sharing a graph keep separate memories ("org|graph").
func (s *Server) memNamespace(r *http.Request, graph string) (string, error) {
	org, err := s.repoOrg(r, graph)
	if err != nil || org == "" {
		return graph, err
	}
	return memory.Namespace(org, graph), nil
}

// seesReview checks private content against the granted tenant entry.
func seesReview(sc scope, graph, orgID string) bool {
	if sc.all {
		return true
	}
	if reach, ok := sc.reach[graph]; ok && reach.OrgID == orgID {
		return !reach.Ambiguous
	}
	return false
}
