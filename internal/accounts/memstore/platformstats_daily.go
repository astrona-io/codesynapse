package memstore

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// dailyTable holds the daily platform statistics. Like jobs it is shared by
// a store and its transaction snapshots.
type dailyTable struct {
	mu   sync.Mutex
	rows map[time.Time][]accounts.PlatformStat
}

func (s *Store) dailyTable() *dailyTable {
	s.dailyOnce.Do(func() {
		if s.daily == nil {
			s.daily = &dailyTable{rows: map[time.Time][]accounts.PlatformStat{}}
		}
	})
	return s.daily
}

func before(t *time.Time, end time.Time) bool { return t != nil && t.Before(end) }

func within(t *time.Time, start, end time.Time) bool {
	return t != nil && !t.Before(start) && t.Before(end)
}

// CountPlatformStats implements accounts.Store, with the rules of the
// PostgreSQL queries.
func (s *Store) CountPlatformStats(_ context.Context, day time.Time) ([]accounts.PlatformStat, error) {
	if !day.Equal(accounts.StatDay(day)) {
		return nil, accounts.ErrInvalidInput
	}
	day = accounts.StatDay(day)
	start, end := day, day.AddDate(0, 0, 1)
	set := accounts.NewPlatformStatSet(day)
	s.mu.RLock()
	defer s.mu.RUnlock()

	counted := func(userID string) bool {
		u, ok := s.users[userID]
		return ok && !u.BootstrapRoot
	}
	suspended := func(u accounts.User) bool { return before(u.DisabledAt, end) }

	// Users, and per user their memberships for guest-only and multi-org.
	type userMembers struct {
		n         int
		guestOnly bool
	}
	perUser := map[string]*userMembers{}
	seated := map[string]bool{}
	for orgID, ms := range s.members {
		for userID, m := range ms {
			u, ok := s.users[userID]
			if !ok || u.BootstrapRoot || !m.addedAt.Before(end) {
				continue
			}
			um := perUser[userID]
			if um == nil {
				um = &userMembers{guestOnly: true}
				perUser[userID] = um
			}
			um.n++
			um.guestOnly = um.guestOnly && m.role == accounts.Guest
			set.Both(orgID, accounts.StatMemberships, "total", 1)
			set.Both(orgID, accounts.StatMemberships, string(m.role), 1)
			if m.role != accounts.Guest && !suspended(u) {
				set.Org(orgID, accounts.StatSeats, "total", 1)
				seated[userID] = true
			}
		}
	}
	set.Total(accounts.StatSeats, "total", int64(len(seated)))
	for _, u := range s.users {
		if u.BootstrapRoot || !u.CreatedAt.Before(end) {
			continue
		}
		set.Total(accounts.StatUsers, "total", 1)
		if !u.CreatedAt.Before(start) {
			set.Total(accounts.StatUsers, "created", 1)
		}
		if suspended(u) {
			set.Total(accounts.StatUsers, "suspended", 1)
		}
		if u.PlatformAdmin {
			set.Total(accounts.StatUsers, "platform_admins", 1)
		}
		if um := perUser[u.ID]; um != nil {
			if um.guestOnly {
				set.Total(accounts.StatUsers, "guest_only", 1)
			}
			if um.n > 1 {
				set.Total(accounts.StatUsers, "multi_org", 1)
			}
		}
	}

	// Organisations.
	for id, o := range s.orgs {
		if !o.CreatedAt.Before(end) {
			continue
		}
		set.Total(accounts.StatOrgs, "total", 1)
		if !o.CreatedAt.Before(start) {
			set.Total(accounts.StatOrgs, "created", 1)
		}
		members := false
		for userID, m := range s.members[id] {
			if counted(userID) && m.addedAt.Before(end) {
				members = true
				break
			}
		}
		if !members {
			set.Total(accounts.StatOrgs, "no_members", 1)
		}
		if _, ok := s.idps[id]; ok {
			set.Total(accounts.StatOrgs, "sso", 1)
		}
	}

	// Active users per window.
	for dim, days := range accounts.StatActiveWindows {
		from := end.AddDate(0, 0, -days)
		active := map[string]bool{}
		for _, e := range s.events {
			if e.Action == "user.login" && e.Outcome == accounts.Success && e.UserID != "" && !e.At.Before(from) && e.At.Before(end) {
				active[e.UserID] = true
			}
		}
		for _, se := range s.sessions {
			if within(&se.CreatedAt, from, end) {
				active[se.UserID] = true
			}
		}
		for _, k := range s.keys {
			if within(k.LastUsedAt, from, end) {
				active[k.UserID] = true
			}
		}
		for _, x := range s.executions {
			if within(&x.CreatedAt, from, end) {
				active[x.UserID] = true
			}
		}
		n := int64(0)
		for id := range active {
			if counted(id) {
				n++
			}
		}
		set.Total(accounts.StatActiveUsers, dim, n)
	}

	// Repository entries and distinct graphs.
	graphs := map[string]bool{}
	for _, r := range s.repos {
		if !r.AddedAt.Before(end) {
			continue
		}
		graphs[r.Graph] = true
		set.Both(r.OrgID, accounts.StatRepoEntries, "total", 1)
		if r.Personal() {
			set.Both(r.OrgID, accounts.StatRepoEntries, "personal", 1)
		} else {
			set.Both(r.OrgID, accounts.StatRepoEntries, "organisation", 1)
		}
		set.Both(r.OrgID, accounts.StatRepoEntries, r.Visibility, 1)
		if r.Access == accounts.AccessLost {
			set.Both(r.OrgID, accounts.StatRepoEntries, "lost", 1)
		}
	}
	set.Total(accounts.StatGraphs, "total", int64(len(graphs)))

	// Browser sessions.
	for _, se := range s.sessions {
		if !counted(se.UserID) || !se.CreatedAt.Before(end) {
			continue
		}
		if se.ExpiresAt.After(end) {
			set.Total(accounts.StatBrowserSessions, "active", 1)
		}
		if !se.CreatedAt.Before(start) {
			set.Total(accounts.StatBrowserSessions, "created", 1)
		}
	}

	// Execution sessions.
	for _, x := range s.executions {
		if !counted(x.UserID) || !x.CreatedAt.Before(end) {
			continue
		}
		if x.ExpiresAt.After(end) && !before(x.RevokedAt, end) {
			set.Both(x.OrgID, accounts.StatExecutionSessions, "active", 1)
		}
		if !x.CreatedAt.Before(start) {
			set.Both(x.OrgID, accounts.StatExecutionSessions, "created", 1)
			switch {
			case strings.HasPrefix(x.ID, "web:"):
				set.Both(x.OrgID, accounts.StatExecutionSessions, "created_dashboard", 1)
			case strings.HasPrefix(x.ID, "gw:"):
				set.Both(x.OrgID, accounts.StatExecutionSessions, "created_gateway", 1)
			default:
				set.Both(x.OrgID, accounts.StatExecutionSessions, "created_runtime", 1)
			}
		}
		if within(x.RevokedAt, start, end) {
			set.Both(x.OrgID, accounts.StatExecutionSessions, "revoked", 1)
		}
	}

	// API keys.
	for _, k := range s.keys {
		if !counted(k.UserID) || !k.CreatedAt.Before(end) {
			continue
		}
		active := !before(k.RevokedAt, end) && (k.ExpiresAt == nil || k.ExpiresAt.After(end))
		if active {
			set.Both(k.OrgID, accounts.StatAPIKeys, "active", 1)
			if k.ExpiresAt != nil && !k.ExpiresAt.After(end.AddDate(0, 0, 14)) {
				set.Both(k.OrgID, accounts.StatAPIKeys, "expiring_14d", 1)
			}
		}
		if within(k.LastUsedAt, start, end) {
			set.Both(k.OrgID, accounts.StatAPIKeys, "used", 1)
		}
	}

	// Jobs that finished on the day; organisation rows only for
	// organisations that still exist.
	jt := s.jobTable()
	jt.mu.Lock()
	for _, j := range jt.jobs {
		dim, ok := accounts.StatJobDimension(j.Kind, j.State)
		if !ok || !within(j.FinishedAt, start, end) {
			continue
		}
		set.Total(accounts.StatJobsFinished, dim, 1)
		if _, ok := s.orgs[j.OrgID]; ok {
			set.Org(j.OrgID, accounts.StatJobsFinished, dim, 1)
		}
	}
	jt.mu.Unlock()
	return set.Rows(), nil
}

// PutPlatformStats implements accounts.Store.
func (s *Store) PutPlatformStats(_ context.Context, day time.Time, rows []accounts.PlatformStat) error {
	rows, err := accounts.PreparePlatformStats(day, rows)
	if err != nil {
		return err
	}
	t := s.dailyTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rows[day] = rows
	return nil
}

// HasPlatformStats implements accounts.Store.
func (s *Store) HasPlatformStats(_ context.Context, day time.Time) (bool, error) {
	t := s.dailyTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.rows[accounts.StatDay(day)]) > 0, nil
}

// ListPlatformStats implements accounts.Store.
func (s *Store) ListPlatformStats(_ context.Context, q accounts.PlatformStatQuery) ([]accounts.PlatformStat, error) {
	q, err := accounts.PreparePlatformStatQuery(q)
	if err != nil {
		return nil, err
	}
	t := s.dailyTable()
	t.mu.Lock()
	out := []accounts.PlatformStat{}
	for _, rows := range t.rows {
		for _, r := range rows {
			if accounts.MatchPlatformStat(q, r) {
				out = append(out, r)
			}
		}
	}
	t.mu.Unlock()
	accounts.SortPlatformStats(out)
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// PrunePlatformStats implements accounts.Store.
func (s *Store) PrunePlatformStats(_ context.Context, before time.Time) (int, error) {
	t := s.dailyTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for day, rows := range t.rows {
		if day.Before(before) {
			n += len(rows)
			delete(t.rows, day)
		}
	}
	return n, nil
}
