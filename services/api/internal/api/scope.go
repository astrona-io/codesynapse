package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/accounts"
	"github.com/parisnakitakejser/codesynapse/internal/gitx"
	"github.com/parisnakitakejser/codesynapse/internal/model"
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
)

type policy struct {
	kind  policyKind
	least accounts.Role
}

var (
	anyone       = policy{kind: open}
	visibleOnly  = policy{kind: listed}
	platformOnly = policy{kind: platform}
)

func repoRole(r accounts.Role) policy   { return policy{kind: onRepo, least: r} }
func reviewRole(r accounts.Role) policy { return policy{kind: onReview, least: r} }
func orgRole(r accounts.Role) policy    { return policy{kind: inOrg, least: r} }

// scope is what a caller may see: every graph (platform admins, the service
// token, single-tenant mode) or the graphs of their organisations with the
// best role they hold on each.
type scope struct {
	all    bool
	graphs map[string]accounts.Role
	// orgs are the organisations the request acts in: the key's, the one
	// named by OrgHeader, or all of a user's.
	orgs []accounts.Membership
}

type scopeKey struct{}

func scopeFrom(ctx context.Context) scope {
	sc, ok := ctx.Value(scopeKey{}).(scope)
	if !ok {
		return scope{all: true} // single-tenant mode
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

// scopeOf computes the caller's scope.
func (s *Server) scopeOf(r *http.Request, p accounts.Principal) (scope, error) {
	if p.User.PlatformAdmin && p.OrgID == "" && r.Header.Get(OrgHeader) == "" {
		return scope{all: true}, nil
	}
	ctx := r.Context()
	ms, err := s.cfg.Accounts.St.UserMemberships(ctx, p.User.ID)
	if err != nil && p.Via != "service" {
		return scope{}, err
	}
	want := r.Header.Get(OrgHeader)
	sc := scope{graphs: map[string]accounts.Role{}}
	for _, m := range ms {
		if (p.OrgID != "" && m.Org.ID != p.OrgID) || (want != "" && m.Org.Slug != want) {
			continue
		}
		sc.orgs = append(sc.orgs, m)
	}
	if want != "" && len(sc.orgs) == 0 {
		if p.User.PlatformAdmin {
			o, err := s.cfg.Accounts.St.OrgBySlug(ctx, want)
			if err != nil {
				return scope{}, err
			}
			sc.orgs = append(sc.orgs, accounts.Membership{Org: o, User: p.User, Role: accounts.Owner})
		} else {
			return scope{}, accounts.ErrNotFound
		}
	}
	for _, m := range sc.orgs {
		repos, err := s.cfg.Accounts.St.OrgRepositories(ctx, m.Org.ID)
		if err != nil {
			return scope{}, err
		}
		for _, repo := range repos {
			if cur, ok := sc.graphs[repo.Graph]; !ok || m.Role.AtLeast(cur) {
				sc.graphs[repo.Graph] = m.Role
			}
		}
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
			h(w, r) // public routes: the auth layer let them through
			return
		}
		sc, err := s.scopeOf(r, p)
		if err != nil {
			s.accountError(w, err)
			return
		}
		ctx := context.WithValue(r.Context(), scopeKey{}, sc)
		r = r.WithContext(ctx)
		if sc.all {
			h(w, r)
			return
		}
		switch pol.kind {
		case platform:
			s.problem(w, http.StatusForbidden, errors.New("platform admins only"))
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
			if !s.allowRepo(w, sc, rv.Repo, pol.least) {
				return
			}
		case inOrg:
			m, err := sc.selected()
			if err != nil {
				s.problem(w, http.StatusBadRequest, err)
				return
			}
			if !m.Role.AtLeast(pol.least) {
				s.problem(w, http.StatusForbidden, errors.New("your role in "+m.Org.Name+" does not allow this"))
				return
			}
		}
		h(w, r)
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

// linkRepo records that the request's organisation uses graph. Platform
// admins without a selected organisation create no link: the graph stays
// unassigned until they attach it.
func (s *Server) linkRepo(r *http.Request, graph, remote, visibility string) error {
	if s.cfg.Accounts == nil {
		return nil
	}
	sc := scopeFrom(r.Context())
	if sc.all {
		return nil
	}
	m, err := sc.selected()
	if err != nil {
		return err
	}
	p, _ := principal(r.Context())
	err = s.cfg.Accounts.St.AddRepository(r.Context(), accounts.Repository{OrgID: m.Org.ID, Graph: graph, Remote: remote,
		Visibility: visibility, AddedBy: p.User.ID, AddedAt: time.Now().UTC().Truncate(time.Microsecond)})
	if errors.Is(err, accounts.ErrExists) {
		return nil // already linked: adding it again is fine
	}
	return err
}

// publicRemote reports whether remote can be read without credentials,
// which is what lets an organisation share an existing graph or clone it
// without the installation's tokens.
func publicRemote(ctx context.Context, remote, branch string) bool {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err := gitx.RemoteHead(ctx, remote, branch, gitx.Auth{})
	return err == nil
}

type attachRequest struct {
	Graph string `json:"graph"`
}

// handleAttachRepo lets a platform admin assign an existing graph to an
// organisation (graphs indexed before accounts existed have no owner).
func (s *Server) handleAttachRepo(w http.ResponseWriter, r *http.Request) {
	if !s.platformAdmin(w, r) {
		return
	}
	o, err := s.cfg.Accounts.St.OrgBySlug(r.Context(), r.PathValue("org"))
	if err != nil {
		s.accountError(w, err)
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
	vis := accounts.Local
	if n.URL != "" {
		vis = accounts.Private
		if publicRemote(r.Context(), n.URL, n.Branch) {
			vis = accounts.Public
		}
	}
	p, _ := principal(r.Context())
	err = s.cfg.Accounts.St.AddRepository(r.Context(), accounts.Repository{OrgID: o.ID, Graph: req.Graph, Remote: n.URL, Visibility: vis,
		AddedBy: p.User.ID, AddedAt: time.Now().UTC().Truncate(time.Microsecond)})
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
	writeJSON(w, 200, map[string]any{"org": o, "repositories": repos})
}
