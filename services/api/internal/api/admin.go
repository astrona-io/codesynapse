package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/store"
)

// Platform administration (SaaS phase 4): organisations and users across
// the installation, suspending accounts, the platform administrator flag
// and system health. Platform admins only; every change is audited by the
// service and the HTTP middleware.

// AdminOrg is an organisation with its size.
type AdminOrg struct {
	accounts.Org
	Members      int            `json:"members"`
	Roles        map[string]int `json:"roles"`
	Repositories int            `json:"repositories"`
	LostAccess   int            `json:"lost_access"`
	Teams        int            `json:"teams"`
}

// AdminUser is a user with their organisations and keys.
type AdminUser struct {
	accounts.User
	Orgs       []AdminMembership `json:"orgs"`
	ActiveKeys int               `json:"active_keys"`
	// LastKeyUse is the most recent use of any of the user's keys.
	LastKeyUse *time.Time `json:"last_key_use,omitempty"`
}

// AdminMembership names an organisation and the role in it.
type AdminMembership struct {
	Slug string        `json:"slug"`
	Name string        `json:"name"`
	Role accounts.Role `json:"role"`
}

func (s *Server) handleAdminOrgsDetail(w http.ResponseWriter, r *http.Request) {
	if !s.platformAdmin(w, r) {
		return
	}
	ctx := r.Context()
	orgs, err := s.cfg.Accounts.St.ListOrgs(ctx, 1000)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	out := make([]AdminOrg, 0, len(orgs))
	for _, o := range orgs {
		a := AdminOrg{Org: o, Roles: map[string]int{}}
		ms, err := s.cfg.Accounts.St.OrgMembers(ctx, o.ID)
		if err != nil {
			s.problem(w, 500, err)
			return
		}
		a.Members = len(ms)
		for _, m := range ms {
			a.Roles[string(m.Role)]++
		}
		repos, err := s.cfg.Accounts.St.OrgRepositories(ctx, o.ID)
		if err != nil {
			s.problem(w, 500, err)
			return
		}
		a.Repositories = len(repos)
		for _, repo := range repos {
			if !repo.Usable() {
				a.LostAccess++
			}
		}
		teams, err := s.cfg.Accounts.St.OrgTeams(ctx, o.ID)
		if err != nil {
			s.problem(w, 500, err)
			return
		}
		a.Teams = len(teams)
		out = append(out, a)
	}
	writeJSON(w, 200, map[string]any{"orgs": out})
}

func (s *Server) handleAdminUsersDetail(w http.ResponseWriter, r *http.Request) {
	if !s.platformAdmin(w, r) {
		return
	}
	ctx := r.Context()
	users, err := s.cfg.Accounts.St.ListUsers(ctx, 1000)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	out := make([]AdminUser, 0, len(users))
	for _, u := range users {
		u.PasswordHash = ""
		a := AdminUser{User: u, Orgs: []AdminMembership{}}
		ms, err := s.cfg.Accounts.St.UserMemberships(ctx, u.ID)
		if err != nil {
			s.problem(w, 500, err)
			return
		}
		for _, m := range ms {
			a.Orgs = append(a.Orgs, AdminMembership{Slug: m.Org.Slug, Name: m.Org.Name, Role: m.Role})
		}
		keys, err := s.cfg.Accounts.St.ListAPIKeys(ctx, u.ID)
		if err != nil {
			s.problem(w, 500, err)
			return
		}
		for _, k := range keys {
			if k.RevokedAt == nil && (k.ExpiresAt == nil || time.Now().Before(*k.ExpiresAt)) {
				a.ActiveKeys++
			}
			if k.LastUsedAt != nil && (a.LastKeyUse == nil || k.LastUsedAt.After(*a.LastKeyUse)) {
				t := *k.LastUsedAt
				a.LastKeyUse = &t
			}
		}
		out = append(out, a)
	}
	writeJSON(w, 200, map[string]any{"users": out})
}

func (s *Server) adminUserAction(w http.ResponseWriter, r *http.Request, act func(context.Context, accounts.Principal, string) error) {
	if !s.platformAdmin(w, r) {
		return
	}
	p, _ := principal(r.Context())
	if err := act(r.Context(), p, r.PathValue("id")); err != nil {
		s.accountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSuspendUser(w http.ResponseWriter, r *http.Request) {
	s.adminUserAction(w, r, s.cfg.Accounts.SuspendUser)
}

func (s *Server) handleResumeUser(w http.ResponseWriter, r *http.Request) {
	s.adminUserAction(w, r, s.cfg.Accounts.ResumeUser)
}

type platformAdminRequest struct {
	Admin bool `json:"admin"`
}

func (s *Server) handleSetPlatformAdmin(w http.ResponseWriter, r *http.Request) {
	var req platformAdminRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, 400, err)
		return
	}
	s.adminUserAction(w, r, func(ctx context.Context, p accounts.Principal, id string) error {
		return s.cfg.Accounts.SetPlatformAdmin(ctx, p, id, req.Admin)
	})
}

// Health is GET /api/v1/admin/health.
type Health struct {
	Version  string       `json:"version"`
	Checked  string       `json:"checked"`
	Graph    HealthCheck  `json:"graph"`
	Accounts HealthCheck  `json:"accounts"`
	Metrics  HealthCheck  `json:"metrics"`
	Audit    HealthCheck  `json:"audit"`
	Counts   HealthCounts `json:"counts"`
	Jobs     JobHealth    `json:"jobs"`
	Unowned  []string     `json:"unowned"`
}

// HealthCheck is one dependency's state.
type HealthCheck struct {
	OK      bool    `json:"ok"`
	Detail  string  `json:"detail"`
	Seconds float64 `json:"seconds"`
}

// HealthCounts sizes the installation.
type HealthCounts struct {
	Users       int `json:"users"`
	Suspended   int `json:"suspended"`
	Admins      int `json:"admins"`
	Orgs        int `json:"orgs"`
	Graphs      int `json:"graphs"`
	AuditEvents int `json:"audit_events_checked"`
}

func timed(f func() (string, error)) HealthCheck {
	start := time.Now()
	detail, err := f()
	c := HealthCheck{OK: err == nil, Detail: detail, Seconds: time.Since(start).Seconds()}
	if err != nil {
		c.Detail = err.Error()
	}
	return c
}

func (s *Server) handleAdminHealth(w http.ResponseWriter, r *http.Request) {
	if !s.platformAdmin(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	h := Health{Version: s.cfg.Version, Checked: time.Now().UTC().Format(time.RFC3339), Unowned: []string{}}
	var graphs []string
	h.Graph = timed(func() (string, error) {
		repos, err := s.st.Repos(ctx)
		for _, n := range repos {
			graphs = append(graphs, n.Name)
		}
		return store.System(s.st) + ", " + plural(len(repos), "repository", "repositories"), err
	})
	h.Counts.Graphs = len(graphs)
	h.Accounts = timed(func() (string, error) {
		users, err := s.cfg.Accounts.St.ListUsers(ctx, 100000)
		if err != nil {
			return "", err
		}
		h.Counts.Users = len(users)
		for _, u := range users {
			if u.DisabledAt != nil {
				h.Counts.Suspended++
			}
			if u.PlatformAdmin {
				h.Counts.Admins++
			}
		}
		orgs, err := s.cfg.Accounts.St.ListOrgs(ctx, 100000)
		h.Counts.Orgs = len(orgs)
		return "PostgreSQL", err
	})
	h.Metrics = timed(func() (string, error) {
		if s.cfg.Metrics == nil {
			return "", errors.New("not configured: the live statistics are off (CODESYNAPSE_METRICS_URL)")
		}
		_, err := s.cfg.Metrics.QueryRange(ctx, "vector(1)", time.Now().Add(-time.Minute), time.Now(), time.Minute, nil)
		return "VictoriaMetrics", err
	})
	h.Audit = timed(func() (string, error) {
		v, err := accounts.VerifyAudit(ctx, s.cfg.Accounts.St)
		if err != nil {
			return "", err
		}
		h.Counts.AuditEvents = v.Checked
		if !v.OK {
			return v.Reason, errors.New("the audit chain does not verify: " + v.Reason)
		}
		return plural(v.Checked, "entry verifies", "entries verify"), nil
	})
	h.Jobs = jobHealth(s.jobs.List())
	h.Jobs.RecentFailures = []Job{}
	writeJSON(w, 200, h)
}
