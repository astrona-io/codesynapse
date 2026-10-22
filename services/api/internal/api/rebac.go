package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/parisnakitakejser/codesynapse/internal/accounts"
)

// Relationship-based access (accounts/authz.go): teams, repository grants
// and shares. The service checks every rule; these handlers resolve names
// (organisation and team slugs, emails) and shape answers.

type teamView struct {
	accounts.Team
	Role    accounts.Role         `json:"role,omitempty"`
	Members []accounts.TeamMember `json:"members"`
}

func (s *Server) team(w http.ResponseWriter, r *http.Request, o accounts.Org) (accounts.Team, bool) {
	t, err := s.cfg.Accounts.St.TeamBySlug(r.Context(), o.ID, r.PathValue("team"))
	if err != nil {
		s.accountError(w, err)
		return t, false
	}
	return t, true
}

func (s *Server) handleTeams(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.Viewer)
	if !ok {
		return
	}
	teams, err := s.cfg.Accounts.St.OrgTeams(r.Context(), o.ID)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	out := make([]teamView, 0, len(teams))
	for _, t := range teams {
		role, err := s.cfg.Accounts.TeamRole(r.Context(), t)
		if err != nil {
			s.problem(w, 500, err)
			return
		}
		members, err := s.cfg.Accounts.TeamMembers(r.Context(), p, t.ID)
		if err != nil {
			s.accountError(w, err)
			return
		}
		out = append(out, teamView{Team: t, Role: role, Members: members})
	}
	writeJSON(w, 200, map[string]any{"org": o, "teams": out})
}

type nameRequest struct {
	Name string `json:"name"`
}

func (s *Server) handleCreateTeam(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.Viewer)
	if !ok {
		return
	}
	var req nameRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, 400, err)
		return
	}
	t, err := s.cfg.Accounts.CreateTeam(r.Context(), p, o.ID, req.Name)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) handleDeleteTeam(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.Viewer)
	if !ok {
		return
	}
	t, ok := s.team(w, r, o)
	if !ok {
		return
	}
	if err := s.cfg.Accounts.DeleteTeam(r.Context(), p, t.ID); err != nil {
		s.accountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type teamMemberRequest struct {
	Email    string `json:"email"`
	Relation string `json:"relation"`
}

func (s *Server) handleSetTeamMember(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.Guest)
	if !ok {
		return
	}
	t, ok := s.team(w, r, o)
	if !ok {
		return
	}
	var req teamMemberRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, 400, err)
		return
	}
	m, err := s.cfg.Accounts.SetTeamMember(r.Context(), p, t.ID, req.Email, cmpOr(req.Relation, accounts.RelMember))
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, 200, m)
}

func (s *Server) handleRemoveTeamMember(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.Guest)
	if !ok {
		return
	}
	t, ok := s.team(w, r, o)
	if !ok {
		return
	}
	if err := s.cfg.Accounts.RemoveTeamMember(r.Context(), p, t.ID, r.PathValue("user")); err != nil {
		s.accountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type roleRequest struct {
	Role accounts.Role `json:"role"`
}

func (s *Server) handleSetTeamRole(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.Viewer)
	if !ok {
		return
	}
	t, ok := s.team(w, r, o)
	if !ok {
		return
	}
	var req roleRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, 400, err)
		return
	}
	if err := s.cfg.Accounts.SetTeamRole(r.Context(), p, t.ID, req.Role); err != nil {
		s.accountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// grantRequest names a subject by email ("user") or as "org/team" ("team").
type grantRequest struct {
	User string        `json:"user,omitempty"`
	Team string        `json:"team,omitempty"`
	Role accounts.Role `json:"role"`
}

// subject resolves a grant's subject.
func (s *Server) subject(r *http.Request, user, team string) (accounts.Tuple, error) {
	ctx := r.Context()
	switch {
	case user != "" && team == "":
		u, err := s.cfg.Accounts.St.UserByEmail(ctx, accounts.NormalizeEmail(user))
		if err != nil {
			return accounts.Tuple{}, err
		}
		return accounts.Tuple{SubjectType: accounts.TypeUser, SubjectID: u.ID}, nil
	case team != "" && user == "":
		orgSlug, teamSlug, ok := strings.Cut(team, "/")
		if !ok {
			return accounts.Tuple{}, errors.New("name a team as organisation/team")
		}
		o, err := s.cfg.Accounts.St.OrgBySlug(ctx, orgSlug)
		if err != nil {
			return accounts.Tuple{}, err
		}
		t, err := s.cfg.Accounts.St.TeamBySlug(ctx, o.ID, teamSlug)
		if err != nil {
			return accounts.Tuple{}, err
		}
		return accounts.Tuple{SubjectType: accounts.TypeTeam, SubjectID: t.ID, SubjectRelation: accounts.RelMember}, nil
	}
	return accounts.Tuple{}, errors.New("give either a user's email or a team")
}

func (s *Server) handleRepoGrants(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgForAny(w, r)
	if !ok {
		return
	}
	gs, err := s.cfg.Accounts.RepoGrants(r.Context(), p, o.ID, r.PathValue("repo"))
	if err != nil {
		s.accountError(w, err)
		return
	}
	effective, err := s.cfg.Accounts.ResolveRepository(r.Context(), p, o.ID, r.PathValue("repo"), accounts.Viewer)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"org": o.Slug, "repo": r.PathValue("repo"), "grants": gs, "default_repo_access": o.DefaultRepoAccess, "effective": effective})
}

func (s *Server) handleRepoPolicy(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.Admin)
	if !ok {
		return
	}
	if r.Method == http.MethodPut {
		var req struct {
			DefaultRepoAccess string `json:"default_repo_access"`
		}
		if err := decodeJSON(w, r, &req); err != nil {
			s.problem(w, http.StatusBadRequest, err)
			return
		}
		if err := s.cfg.Accounts.SetDefaultRepoAccess(r.Context(), p, o.ID, req.DefaultRepoAccess); err != nil {
			s.accountError(w, err)
			return
		}
		var err error
		o, err = s.cfg.Accounts.St.OrgByID(r.Context(), o.ID)
		if err != nil {
			s.accountError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, o)
}

func (s *Server) handleAccessChanges(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.Owner)
	if !ok {
		return
	}
	role, err := s.cfg.Accounts.St.MemberRole(r.Context(), o.ID, p.User.ID)
	if err != nil || role != accounts.Owner || p.Via != "session" {
		s.accountError(w, accounts.ErrForbidden)
		return
	}
	report, err := s.cfg.Accounts.AccessChangeReport(r.Context(), o.ID)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) handleRecoverRepoAdmin(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.Owner)
	if !ok {
		return
	}
	var req struct {
		User string `json:"user"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	u, err := s.cfg.Accounts.St.UserByEmail(r.Context(), accounts.NormalizeEmail(req.User))
	if err == nil {
		err = s.cfg.Accounts.RecoverRepoAdmin(r.Context(), p, o.ID, r.PathValue("repo"), u.ID)
	}
	if err != nil {
		s.accountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSetRepoGrant(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgForAny(w, r)
	if !ok {
		return
	}
	var req grantRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, 400, err)
		return
	}
	sub, err := s.subject(r, req.User, req.Team)
	if err != nil {
		if errors.Is(err, accounts.ErrNotFound) {
			s.problem(w, http.StatusNotFound, errors.New("no account or team by that name"))
			return
		}
		s.problem(w, 400, err)
		return
	}
	g, err := s.cfg.Accounts.SetRepoGrant(r.Context(), p, o.ID, r.PathValue("repo"), sub, req.Role)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, 200, g)
}

func (s *Server) handleRemoveRepoGrant(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgForAny(w, r)
	if !ok {
		return
	}
	typ, id, _ := strings.Cut(r.URL.Query().Get("subject"), ":")
	sub := accounts.Tuple{SubjectType: typ, SubjectID: id}
	if typ == accounts.TypeTeam {
		sub.SubjectRelation = accounts.RelMember
	}
	if err := s.cfg.Accounts.RemoveRepoGrant(r.Context(), p, o.ID, r.PathValue("repo"), sub); err != nil {
		s.accountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// orgForAny resolves {org} for routes the service authorises itself
// (repository admins need not be organisation members' admins).
func (s *Server) orgForAny(w http.ResponseWriter, r *http.Request) (accounts.Principal, accounts.Org, bool) {
	p, ok := s.caller(w, r)
	if !ok {
		return p, accounts.Org{}, false
	}
	o, err := s.cfg.Accounts.St.OrgBySlug(r.Context(), r.PathValue("org"))
	if err != nil {
		s.accountError(w, err)
		return p, o, false
	}
	return p, o, true
}

type reachView struct {
	accounts.RepoAccess
	Org    string `json:"org"` // slug
	Shared bool   `json:"shared"`
}

// handleMyAccess lists what the caller reaches and why, including
// repositories other organisations shared with them.
func (s *Server) handleMyAccess(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	a, err := s.cfg.Accounts.AccessOf(r.Context(), p.User.ID)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	out := make([]reachView, 0, len(a.Repos))
	slugs := map[string]string{}
	for _, ra := range a.Repos {
		if p.OrgID != "" && ra.OrgID != p.OrgID {
			continue
		}
		if _, ok := slugs[ra.OrgID]; !ok {
			if o, err := s.cfg.Accounts.St.OrgByID(r.Context(), ra.OrgID); err == nil {
				slugs[ra.OrgID] = o.Slug
			}
		}
		_, member := a.Orgs[ra.OrgID]
		out = append(out, reachView{RepoAccess: ra, Org: slugs[ra.OrgID], Shared: !member})
	}
	if p.OrgID != "" {
		for org := range a.Orgs {
			if org != p.OrgID {
				delete(a.Orgs, org)
			}
		}
		for team := range a.Teams {
			t, err := s.cfg.Accounts.St.TeamByID(r.Context(), team)
			if err != nil || t.OrgID != p.OrgID {
				delete(a.Teams, team)
			}
		}
	}
	writeJSON(w, 200, map[string]any{"orgs": a.Orgs, "teams": a.Teams, "repos": out})
}
