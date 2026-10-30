package accounts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Platform statistics (docs/plans/PLATFORM_STATS_PLAN.md): counts across every
// organisation for platform administrators. The figures are aggregates
// only: organisation names and slugs may appear, never repository names,
// graph IDs, e-mail addresses or user, key or session IDs. Root is never
// counted as a user, member, active user or seat.

// ErrAuditUnavailable means a view could not be recorded in the audit log,
// so its figures are withheld.
var ErrAuditUnavailable = errors.New("audit log unavailable")

// StatsPageSize is the most organisation rows one page of a table holds.
const StatsPageSize = 50

// MaxStatsRows caps the organisation rows a store returns and an export
// writes.
const MaxStatsRows = 10000

// KeyExpiryWindow is how far ahead a key counts as expiring soon.
const KeyExpiryWindow = 14 * 24 * time.Hour

var statsRanges = map[string]time.Duration{
	"1h":  time.Hour,
	"6h":  6 * time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

// statsSections are what PlatformStats records a view of. The admin lists
// and health keep their own actions; the rest are admin.stats.*.
var statsSections = map[string]string{
	"users":    "admin.users.view",
	"orgs":     "admin.orgs.view",
	"health":   "admin.health.view",
	"series":   "admin.stats.view",
	"summary":  "admin.stats.view",
	"tenants":  "admin.stats.view",
	"sessions": "admin.stats.view",
	"usage":    "admin.stats.view",
	"security": "admin.stats.view",
	"audit":    "admin.stats.view",
	"models":   "admin.stats.view",
	"jobs":     "admin.stats.view",
	"ingest":   "admin.stats.view",
	"storage":  "admin.stats.view",
	"alerts":   "admin.stats.view",
}

// StatsRequest names what a platform administrator asks for. Every field is
// checked against an allowlist or PostgreSQL before it reaches the audit log.
type StatsRequest struct {
	Section string
	Range   string // 1h, 6h, 24h, 7d, 30d, 90d or 365d; empty for the admin lists
	OrgID   string // optional organisation filter
	Format  string // csv or json for an export; empty for a view
	Panel   string // the series panel, for section "series"
}

// StatsQuery is a validated window, read by the store's statistics methods.
type StatsQuery struct {
	Since time.Time
	Now   time.Time
	OrgID string
	// Daily names a 90d or 365d range, whose window figures come from the
	// daily rollup (PlatformStatsHistory) rather than from the live tables;
	// Since is then the first day of that range.
	Daily string
}

// PlatformStats allows a platform statistics view and records it before any
// figure is read: a signed-in browser session of a platform administrator
// (re-read from the store), never an API key or the installation token. When
// the audit entry cannot be written it answers ErrAuditUnavailable and the
// caller must not answer with figures.
func (s *Service) PlatformStats(ctx context.Context, p Principal, r StatsRequest) (StatsQuery, error) {
	if err := s.requirePlatform(ctx, p); err != nil {
		return StatsQuery{}, err
	}
	action, ok := statsSections[r.Section]
	if !ok {
		return StatsQuery{}, fmt.Errorf("section: %w", ErrInvalidInput)
	}
	q := StatsQuery{Now: s.now()}
	if r.Range != "" {
		if from, _, ok := DailyStatRange(r.Range, q.Now); ok {
			q.Since, q.Daily = from, r.Range
		} else if d, ok := statsRanges[r.Range]; ok {
			q.Since = q.Now.Add(-d)
		} else {
			return StatsQuery{}, fmt.Errorf("range must be 1h, 6h, 24h, 7d, 30d, 90d or 365d: %w", ErrInvalidInput)
		}
	}
	switch r.Format {
	case "":
	case "csv", "json":
		action = "admin.stats.export"
	default:
		return StatsQuery{}, fmt.Errorf("format must be csv or json: %w", ErrInvalidInput)
	}
	if r.OrgID != "" {
		if len(r.OrgID) > 64 {
			return StatsQuery{}, fmt.Errorf("org: %w", ErrInvalidInput)
		}
		if _, err := s.St.OrgByID(ctx, r.OrgID); err != nil {
			if errors.Is(err, ErrNotFound) {
				return StatsQuery{}, fmt.Errorf("unknown organisation: %w", ErrInvalidInput)
			}
			return StatsQuery{}, err
		}
		q.OrgID = r.OrgID
	}
	var target []string
	for _, kv := range [][2]string{{"section", r.Section}, {"panel", r.Panel}, {"range", r.Range}, {"org", r.OrgID}, {"format", r.Format}} {
		if kv[1] != "" {
			target = append(target, kv[0]+"="+kv[1])
		}
	}
	e := actorEvent(p, action, "", strings.Join(target, " "))
	e.Outcome = Success
	if _, err := s.Record(ctx, e); err != nil {
		return StatsQuery{}, fmt.Errorf("%w: %w", ErrAuditUnavailable, err)
	}
	return q, nil
}

// TenantTotals are the platform's organisations, people, memberships,
// repository entries and watches. Users are distinct accounts other than
// root; memberships count each person once per organisation.
type TenantTotals struct {
	Orgs               int            `json:"orgs"`
	OrgsCreated        int            `json:"orgs_created"`
	OrgsWithoutMembers int            `json:"orgs_without_members"`
	OrgsWithSSO        int            `json:"orgs_with_sso"`
	Users              int            `json:"users"`
	UsersCreated       int            `json:"users_created"`
	UsersSuspended     int            `json:"users_suspended"`
	PlatformAdmins     int            `json:"platform_admins"`
	GuestOnlyUsers     int            `json:"guest_only_users"`
	MultiOrgUsers      int            `json:"multi_org_users"`
	ActiveUsers        int            `json:"active_users"`
	Memberships        int            `json:"memberships"`
	Roles              map[string]int `json:"roles"`
	Seats              int            `json:"seats"`
	SeatedPeople       int            `json:"seated_people"`
	Teams              int            `json:"teams"`
	Entries            int            `json:"entries"`
	OrgEntries         int            `json:"org_entries"`
	PersonalEntries    int            `json:"personal_entries"`
	PublicEntries      int            `json:"public_entries"`
	PrivateEntries     int            `json:"private_entries"`
	LocalEntries       int            `json:"local_entries"`
	LostEntries        int            `json:"lost_entries"`
	Graphs             int            `json:"graphs"`
	Watches            int            `json:"watches"`
	WatchAdds          int            `json:"watch_adds"`
}

// OrgTenantRow is one organisation's line of the tenants table.
type OrgTenantRow struct {
	OrgID           string         `json:"org_id"`
	Slug            string         `json:"slug"`
	Name            string         `json:"name"`
	CreatedAt       time.Time      `json:"created_at"`
	Members         int            `json:"members"`
	Roles           map[string]int `json:"roles"`
	Seats           int            `json:"seats"`
	ActiveMembers   int            `json:"active_members"`
	Teams           int            `json:"teams"`
	Entries         int            `json:"entries"`
	PersonalEntries int            `json:"personal_entries"`
	PrivateEntries  int            `json:"private_entries"`
	LostEntries     int            `json:"lost_entries"`
	SSO             bool           `json:"sso"`
	Watches         int            `json:"watches"`
}

// TenantStats is the tenants section: platform totals and, ordered by name
// and ID, at most MaxStatsRows organisation rows (only StatsQuery.OrgID's
// when set).
type TenantStats struct {
	Totals TenantTotals   `json:"totals"`
	Orgs   []OrgTenantRow `json:"orgs"`
}

// ExecutionCounts are runtime sessions: active now, and created and revoked
// in the window. Created splits into dashboard bindings (web:), gateway
// sessions (gw:) and runtime sessions supplied by an agent.
type ExecutionCounts struct {
	Active    int `json:"active"`
	Created   int `json:"created"`
	Revoked   int `json:"revoked"`
	Dashboard int `json:"dashboard"`
	Gateway   int `json:"gateway"`
	Runtime   int `json:"runtime"`
}

// KeyCounts are API keys: active now, used in the window, and expiring
// within KeyExpiryWindow.
type KeyCounts struct {
	Active   int `json:"active"`
	Used     int `json:"used"`
	Expiring int `json:"expiring"`
}

// SessionTotals are the platform's sessions and keys.
type SessionTotals struct {
	BrowserActive  int             `json:"browser_active"`
	BrowserCreated int             `json:"browser_created"`
	Execution      ExecutionCounts `json:"execution"`
	Keys           KeyCounts       `json:"keys"`
}

// OrgSessionRow is one organisation's line of the sessions table. Browser
// sessions belong to no organisation, so only the totals carry them.
type OrgSessionRow struct {
	OrgID     string          `json:"org_id"`
	Slug      string          `json:"slug"`
	Name      string          `json:"name"`
	Execution ExecutionCounts `json:"execution"`
	Keys      KeyCounts       `json:"keys"`
}

// SessionStats is the sessions section, shaped like TenantStats.
type SessionStats struct {
	Totals SessionTotals   `json:"totals"`
	Orgs   []OrgSessionRow `json:"orgs"`
}

// SplitExecution fills the kinds of created runtime sessions from counts of
// all, web: and gw: sessions, so both stores derive them the same way.
func SplitExecution(c *ExecutionCounts) {
	c.Runtime = c.Created - c.Dashboard - c.Gateway
}

// StatsPage returns page (from 1) of rows, StatsPageSize at a time, and the
// number of pages.
func StatsPage[T any](rows []T, page int) ([]T, int) {
	pages := max(1, (len(rows)+StatsPageSize-1)/StatsPageSize)
	page = min(max(page, 1), pages)
	start := (page - 1) * StatsPageSize
	return rows[start:min(start+StatsPageSize, len(rows))], pages
}
