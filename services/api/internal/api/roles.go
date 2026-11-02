package api

import (
	"net/http"
	"strings"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// Custom roles (docs/plans/OPENFGA_PLAN.md, phase 3). The routes only
// resolve the organisation for people who may read its roles; the service
// decides everything else (signed-in session, organisation.roles:full, no
// escalation, audit first).

func (s *Server) handleCustomRoles(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.OrgRolesRead)
	if !ok {
		return
	}
	rs, err := s.cfg.Accounts.CustomRoles(r.Context(), p, o.ID)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"org": o.Slug, "roles": rs, "limit": accounts.MaxCustomRoles})
}

func (s *Server) handleCreateCustomRole(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.OrgRolesRead)
	if !ok {
		return
	}
	var req accounts.CustomRoleInput
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	role, err := s.cfg.Accounts.CreateCustomRole(r.Context(), p, o.ID, req)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, role)
}

func (s *Server) handleUpdateCustomRole(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.OrgRolesRead)
	if !ok {
		return
	}
	var req accounts.CustomRoleInput
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	role, err := s.cfg.Accounts.UpdateCustomRole(r.Context(), p, o.ID, r.PathValue("id"), req)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, role)
}

func (s *Server) handleDeleteCustomRole(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.OrgRolesRead)
	if !ok {
		return
	}
	if err := s.cfg.Accounts.DeleteCustomRole(r.Context(), p, o.ID, r.PathValue("id")); err != nil {
		s.accountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleOrgRoleGrants(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.OrgRolesRead)
	if !ok {
		return
	}
	gs, err := s.cfg.Accounts.OrgRoleGrants(r.Context(), p, o.ID)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"org": o.Slug, "grants": gs})
}

// orgRoleGrantRequest names a member by email or a team as org/team, and
// the custom organisation role to give them.
type orgRoleGrantRequest struct {
	User string `json:"user,omitempty"`
	Team string `json:"team,omitempty"`
	Role string `json:"role"`
}

func (s *Server) handleSetOrgRoleGrant(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.OrgRolesRead)
	if !ok {
		return
	}
	var req orgRoleGrantRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	sub, ok := s.grantSubject(w, r, o.ID, req.User, req.Team)
	if !ok {
		return
	}
	g, err := s.cfg.Accounts.SetOrgRoleGrant(r.Context(), p, o.ID, sub, req.Role)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func (s *Server) handleRemoveOrgRoleGrant(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.OrgRolesRead)
	if !ok {
		return
	}
	typ, id, _ := strings.Cut(r.URL.Query().Get("subject"), ":")
	sub := accounts.Tuple{SubjectType: typ, SubjectID: id}
	if typ == accounts.TypeTeam {
		sub.SubjectRelation = accounts.TeamRead
	}
	if err := s.cfg.Accounts.RemoveOrgRoleGrant(r.Context(), p, o.ID, sub); err != nil {
		s.accountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// permissionInfo is one entry of the permission catalogue the role and key
// scope pickers group by resource and area.
type permissionInfo struct {
	Permission accounts.Permission `json:"permission"`
	Resource   string              `json:"resource"`
	Area       string              `json:"area"`
	Level      string              `json:"level"`
	// Floor is the weakest built-in role that holds it.
	Floor accounts.Role `json:"floor"`
}

// handlePermissionCatalog lists every permission a custom role or key scope
// can name; markers (organisation#member, #owner) are relations, not
// permissions, and are left out.
func (s *Server) handlePermissionCatalog(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.caller(w, r); !ok {
		return
	}
	all := accounts.AllPermissions()
	out := make([]permissionInfo, 0, len(all))
	for _, p := range all {
		if p.Marker() {
			continue
		}
		out = append(out, permissionInfo{Permission: p, Resource: p.Resource(), Area: p.Area(), Level: p.Level(), Floor: p.Floor()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"permissions": out})
}
