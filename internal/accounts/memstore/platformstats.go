package memstore

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

func inWindow(t time.Time, q accounts.StatsQuery) bool {
	return !t.Before(q.Since) && !t.After(q.Now)
}

func inWindowPtr(t *time.Time, q accounts.StatsQuery) bool {
	return t != nil && inWindow(*t, q)
}

// sortedOrgs returns the organisations a statistics query lists, ordered by
// name and ID and capped like the PostgreSQL store.
func (s *Store) sortedOrgs(orgID string) []accounts.Org {
	var orgs []accounts.Org
	for _, o := range s.orgs {
		if orgID == "" || o.ID == orgID {
			orgs = append(orgs, o)
		}
	}
	slices.SortFunc(orgs, func(a, b accounts.Org) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
	})
	return orgs[:min(len(orgs), accounts.MaxStatsRows)]
}

// activeUsers are the accounts with a successful sign-in, a new browser or
// runtime session or a key use in the window.
func (s *Store) activeUsers(q accounts.StatsQuery) map[string]bool {
	active := map[string]bool{}
	for _, e := range s.events {
		if (e.Action == "user.login" || e.Action == "sso.login") && e.Outcome != accounts.Failure && e.UserID != "" && inWindow(e.At, q) {
			active[e.UserID] = true
		}
	}
	for _, x := range s.sessions {
		if inWindow(x.CreatedAt, q) {
			active[x.UserID] = true
		}
	}
	for _, k := range s.keys {
		if inWindowPtr(k.LastUsedAt, q) {
			active[k.UserID] = true
		}
	}
	for _, x := range s.executions {
		if inWindow(x.CreatedAt, q) {
			active[x.UserID] = true
		}
	}
	return active
}

func (s *Store) isRoot(userID string) bool {
	u, ok := s.users[userID]
	return ok && u.BootstrapRoot
}

// TenantStats implements accounts.Store.
func (s *Store) TenantStats(_ context.Context, q accounts.StatsQuery) (accounts.TenantStats, error) {
	watches := map[string]int{}
	t := s.scheduleTable()
	t.mu.Lock()
	for _, w := range t.watches {
		if !w.Paused {
			watches[w.OrgID]++
		}
	}
	t.mu.Unlock()

	s.mu.RLock()
	defer s.mu.RUnlock()
	out := accounts.TenantStats{Totals: accounts.TenantTotals{Roles: map[string]int{}}, Orgs: []accounts.OrgTenantRow{}}
	tot := &out.Totals
	active := s.activeUsers(q)
	for _, o := range s.orgs {
		tot.Orgs++
		if inWindow(o.CreatedAt, q) {
			tot.OrgsCreated++
		}
	}
	for _, idp := range s.idps {
		if idp.Enabled {
			tot.OrgsWithSSO++
		}
	}
	for id, u := range s.users {
		if u.BootstrapRoot {
			continue
		}
		tot.Users++
		if inWindow(u.CreatedAt, q) {
			tot.UsersCreated++
		}
		if u.DisabledAt != nil {
			tot.UsersSuspended++
		}
		if u.PlatformAdmin {
			tot.PlatformAdmins++
		}
		if active[id] {
			tot.ActiveUsers++
		}
	}
	perUser := map[string]int{}
	guestOnly := map[string]bool{}
	seated := map[string]bool{}
	orgMembers := map[string]int{}
	for orgID, ms := range s.members {
		for userID, m := range ms {
			u, ok := s.users[userID]
			if !ok || u.BootstrapRoot {
				continue
			}
			orgMembers[orgID]++
			tot.Memberships++
			tot.Roles[string(m.role)]++
			perUser[userID]++
			if _, seen := guestOnly[userID]; !seen {
				guestOnly[userID] = true
			}
			if m.role != accounts.Guest {
				guestOnly[userID] = false
				if u.DisabledAt == nil {
					tot.Seats++
					seated[userID] = true
				}
			}
		}
	}
	for _, n := range perUser {
		if n > 1 {
			tot.MultiOrgUsers++
		}
	}
	for _, g := range guestOnly {
		if g {
			tot.GuestOnlyUsers++
		}
	}
	tot.SeatedPeople = len(seated)
	for id := range s.orgs {
		if orgMembers[id] == 0 {
			tot.OrgsWithoutMembers++
		}
	}
	tot.Teams = len(s.teams)
	graphs := map[string]bool{}
	for _, r := range s.repos {
		tot.Entries++
		graphs[r.Graph] = true
		if r.OwnerID == "" {
			tot.OrgEntries++
		} else {
			tot.PersonalEntries++
		}
		switch r.Visibility {
		case accounts.Public:
			tot.PublicEntries++
		case accounts.Private:
			tot.PrivateEntries++
		case accounts.Local:
			tot.LocalEntries++
		}
		if r.Access == accounts.AccessLost {
			tot.LostEntries++
		}
	}
	tot.Graphs = len(graphs)
	for _, n := range watches {
		tot.Watches += n
	}
	for _, e := range s.events {
		if e.Action == "watch.repo_add" && e.Outcome == accounts.Success && inWindow(e.At, q) {
			tot.WatchAdds++
		}
	}

	for _, o := range s.sortedOrgs(q.OrgID) {
		row := accounts.OrgTenantRow{OrgID: o.ID, Slug: o.Slug, Name: o.Name, CreatedAt: o.CreatedAt, Roles: map[string]int{}, Watches: watches[o.ID]}
		for userID, m := range s.members[o.ID] {
			u, ok := s.users[userID]
			if !ok || u.BootstrapRoot {
				continue
			}
			row.Members++
			row.Roles[string(m.role)]++
			if m.role != accounts.Guest && u.DisabledAt == nil {
				row.Seats++
			}
			if active[userID] {
				row.ActiveMembers++
			}
		}
		for _, tm := range s.teams {
			if tm.OrgID == o.ID {
				row.Teams++
			}
		}
		for _, r := range s.repos {
			if r.OrgID != o.ID {
				continue
			}
			row.Entries++
			if r.OwnerID != "" {
				row.PersonalEntries++
			}
			if r.Visibility == accounts.Private {
				row.PrivateEntries++
			}
			if r.Access == accounts.AccessLost {
				row.LostEntries++
			}
		}
		row.SSO = s.idps[o.ID].Enabled
		out.Orgs = append(out.Orgs, row)
	}
	return out, nil
}

// SessionStats implements accounts.Store.
func (s *Store) SessionStats(_ context.Context, q accounts.StatsQuery) (accounts.SessionStats, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := accounts.SessionStats{Orgs: []accounts.OrgSessionRow{}}
	tot := &out.Totals
	for _, x := range s.sessions {
		if s.isRoot(x.UserID) {
			continue
		}
		if x.ExpiresAt.After(q.Now) {
			tot.BrowserActive++
		}
		if inWindow(x.CreatedAt, q) {
			tot.BrowserCreated++
		}
	}
	execs := map[string]*accounts.ExecutionCounts{}
	for _, x := range s.executions {
		if s.isRoot(x.UserID) {
			continue
		}
		c := execs[x.OrgID]
		if c == nil {
			c = &accounts.ExecutionCounts{}
			execs[x.OrgID] = c
		}
		if x.RevokedAt == nil && x.ExpiresAt.After(q.Now) {
			c.Active++
		}
		if inWindow(x.CreatedAt, q) {
			c.Created++
			switch {
			case strings.HasPrefix(x.ID, "web:"):
				c.Dashboard++
			case strings.HasPrefix(x.ID, "gw:"):
				c.Gateway++
			}
		}
		if inWindowPtr(x.RevokedAt, q) {
			c.Revoked++
		}
	}
	keys := map[string]*accounts.KeyCounts{}
	for _, k := range s.keys {
		if s.isRoot(k.UserID) {
			continue
		}
		c := keys[k.OrgID]
		if c == nil {
			c = &accounts.KeyCounts{}
			keys[k.OrgID] = c
		}
		live := k.RevokedAt == nil && (k.ExpiresAt == nil || k.ExpiresAt.After(q.Now))
		if live {
			c.Active++
		}
		if inWindowPtr(k.LastUsedAt, q) {
			c.Used++
		}
		if live && k.ExpiresAt != nil && !k.ExpiresAt.After(q.Now.Add(accounts.KeyExpiryWindow)) {
			c.Expiring++
		}
	}
	for _, c := range execs {
		tot.Execution.Active += c.Active
		tot.Execution.Created += c.Created
		tot.Execution.Revoked += c.Revoked
		tot.Execution.Dashboard += c.Dashboard
		tot.Execution.Gateway += c.Gateway
		accounts.SplitExecution(c)
	}
	accounts.SplitExecution(&tot.Execution)
	for _, c := range keys {
		tot.Keys.Active += c.Active
		tot.Keys.Used += c.Used
		tot.Keys.Expiring += c.Expiring
	}
	for _, o := range s.sortedOrgs(q.OrgID) {
		row := accounts.OrgSessionRow{OrgID: o.ID, Slug: o.Slug, Name: o.Name}
		if c := execs[o.ID]; c != nil {
			row.Execution = *c
		}
		if c := keys[o.ID]; c != nil {
			row.Keys = *c
		}
		out.Orgs = append(out.Orgs, row)
	}
	return out, nil
}
