package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/astrona-io/codesynapse/internal/history"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/redact"
)

// Notifications for the dashboard's bell: what happened lately in the
// repositories the caller may see, derived when asked from what is already
// recorded (snapshots after each ingest, jobs, reviews), so no part of the
// system has to remember to send one. Each source keeps its own scope check
// (visibleRepos, seesReview). Only the "seen up to" mark is stored, per
// user, in accounts mode; without accounts the browser keeps it.

const (
	notifyWindow   = 14 * 24 * time.Hour
	notifyMaxRepos = 50
	notifyDefault  = 20
	notifyMax      = 50
	notifyCacheTTL = 30 * time.Second
	notifyCacheMax = 256
	notifyTimeout  = 5 * time.Second
	notifyTextMax  = 200
	// notifySnaps is how many recent snapshots of a repository are read to
	// find the previous one of the latest snapshot's branch.
	notifySnaps     = 20
	notifyBranchMax = 60
)

// Notification is one thing that happened.
type Notification struct {
	ID string `json:"id"`
	// Kind is findings, grade (the security grade fell with no new serious
	// findings), sync_failed or review.
	Kind  string `json:"kind"`
	Tone  string `json:"tone"` // bad, warn, info
	Repo  string `json:"repo"`
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	Link  string `json:"link"`
	At    string `json:"at"`
}

// Notifications is the feed and the caller's seen mark.
type Notifications struct {
	Items []Notification `json:"items"`
	// SeenAt is the caller's mark (accounts mode); Unread counts items after it.
	SeenAt string `json:"seen_at,omitempty"`
	Unread int    `json:"unread"`
}

type notifyCacheEntry struct {
	at    time.Time
	items []Notification
}

// notifyCache keeps a scope's feed briefly: the bell polls, and the feed
// reads every visible repository.
type notifyCache struct {
	mu      sync.Mutex
	entries map[string]notifyCacheEntry
}

func (s *Server) notificationRoutes() {
	s.route("GET /api/v1/notifications", visibleOnly, s.handleNotifications)
	s.route("POST /api/v1/notifications/seen", visibleOnly, s.handleNotificationsSeen)
}

func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	limit := min(max(intParam(r, "limit", notifyDefault), 1), notifyMax)
	ctx, cancel := context.WithTimeout(r.Context(), notifyTimeout)
	defer cancel()
	items, err := s.notificationFeed(ctx)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	out := Notifications{Items: items[:min(len(items), limit)]}
	if p, ok := principal(ctx); ok && s.cfg.Accounts != nil && p.User.ID != "" {
		seen, err := s.cfg.Accounts.St.NotificationsSeen(ctx, p.User.ID)
		if err != nil {
			s.problem(w, 500, err)
			return
		}
		if !seen.IsZero() {
			out.SeenAt = seen.Format(time.RFC3339)
		}
		for _, n := range out.Items {
			if at, err := time.Parse(time.RFC3339, n.At); err == nil && at.After(seen) {
				out.Unread++
			}
		}
	} else {
		out.Unread = len(out.Items) // the browser applies its own mark
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleNotificationsSeen(w http.ResponseWriter, r *http.Request) {
	var in struct {
		At string `json:"at"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		s.problem(w, 400, err)
		return
	}
	at, err := time.Parse(time.RFC3339, in.At)
	if err != nil || at.After(time.Now().Add(time.Minute)) {
		s.problem(w, 400, errors.New("at must be an RFC 3339 time, not in the future"))
		return
	}
	p, ok := principal(r.Context())
	if !ok || s.cfg.Accounts == nil || p.User.ID == "" {
		w.WriteHeader(http.StatusNoContent) // without accounts the browser keeps the mark
		return
	}
	if err := s.cfg.Accounts.St.SetNotificationsSeen(r.Context(), p.User.ID, at); err != nil {
		s.accountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// notificationFeed returns the caller's items, newest first, from the
// cache when the same scope asked moments ago.
func (s *Server) notificationFeed(ctx context.Context) ([]Notification, error) {
	repos, err := s.st.Repos(ctx)
	if err != nil {
		return nil, err
	}
	repos = visibleRepos(ctx, repos)
	// Most recently indexed first, so the limit keeps the active ones.
	slices.SortFunc(repos, func(a, b model.Node) int { return cmp.Or(cmp.Compare(b.Time, a.Time), cmp.Compare(a.Name, b.Name)) })
	repos = repos[:min(len(repos), notifyMaxRepos)]
	sc := scopeFrom(ctx)
	key := feedKey(sc, repos)
	if items, ok := s.notify.get(key); ok {
		return items, nil
	}
	since := time.Now().Add(-notifyWindow)
	var items []Notification
	for _, repo := range repos {
		snaps, err := s.st.Snapshots(ctx, repo.Name, notifySnaps)
		if err != nil {
			return nil, err
		}
		if n, ok := findingsNotification(repo.Name, snaps, since); ok {
			items = append(items, n)
		}
		list, err := s.st.ListReviews(ctx, repo.Name, 5)
		if err != nil {
			return nil, err
		}
		for _, rv := range list {
			at, err := time.Parse(time.RFC3339, rv.CreatedAt)
			if err != nil || at.Before(since) || !seesReview(sc, rv.Repo, rv.OrgID) {
				continue
			}
			by := cmp.Or(rv.Author, rv.Source, "someone")
			items = append(items, Notification{ID: "review:" + rv.ID, Kind: "review", Tone: "info", Repo: rv.Repo,
				Title: clip(redact.Free(fmt.Sprintf("New review: %s", rv.Title)), notifyTextMax), Body: clip(redact.Free("by "+by), notifyTextMax),
				Link: "/repos/" + url.PathEscape(rv.Repo) + "/reviews?id=" + url.QueryEscape(rv.ID), At: at.UTC().Format(time.RFC3339)})
		}
	}
	visible := map[string]bool{}
	for _, r := range repos {
		visible[r.Name] = true
	}
	for _, j := range s.jobs.List(ctx) {
		if j.State != JobFailed || !visible[j.Repo] || !seesReview(sc, j.Repo, j.OrgID) {
			continue
		}
		at, err := time.Parse(time.RFC3339, j.FinishedAt)
		if err != nil || at.Before(since) {
			continue
		}
		what := cmp.Or(j.Kind, "job")
		items = append(items, Notification{ID: "job:" + j.ID, Kind: "sync_failed", Tone: "bad", Repo: j.Repo,
			Title: fmt.Sprintf("%s of %s failed", strings.ToUpper(what[:1])+what[1:], j.Repo), Body: clip(redact.Free(j.Error), notifyTextMax),
			Link: "/repos/" + url.PathEscape(j.Repo), At: at.UTC().Format(time.RFC3339)})
	}
	slices.SortStableFunc(items, func(a, b Notification) int { return cmp.Or(cmp.Compare(b.At, a.At), cmp.Compare(a.ID, b.ID)) })
	items = items[:min(len(items), notifyMax)]
	if items == nil {
		items = []Notification{}
	}
	s.notify.put(key, items)
	return items, nil
}

// findingsNotification alerts on a sync that made security worse: it added
// critical or high findings or vulnerabilities, or the grade fell. The latest
// snapshot is compared with the one before it on the same branch (snaps are
// oldest first), so switching the indexed branch is never called a regression.
func findingsNotification(repo string, snaps []history.Snapshot, since time.Time) (Notification, bool) {
	prev, cur, ok := history.Latest(snaps)
	if !ok {
		return Notification{}, false
	}
	at, err := time.Parse(time.RFC3339, cur.At)
	if err != nil || at.Before(since) {
		return Notification{}, false
	}
	serious := func(m map[string]int) int { return m["critical"] + m["high"] }
	findings := serious(cur.Findings) - serious(prev.Findings)
	vulns := serious(cur.Vulns) - serious(prev.Vulns)
	worse := history.GradeWorse(prev, cur)
	if findings <= 0 && vulns <= 0 && !worse {
		return Notification{}, false
	}
	var parts []string
	if findings > 0 {
		parts = append(parts, plural(findings, "new critical or high finding", "new critical or high findings"))
	}
	if vulns > 0 {
		parts = append(parts, plural(vulns, "new serious vulnerability", "new serious vulnerabilities"))
	}
	grade := ""
	if prev.Grade != "" && cur.Grade != "" && prev.Grade != cur.Grade {
		grade = fmt.Sprintf("Security grade %s → %s", prev.Grade, cur.Grade)
	}
	kind, title, body := "findings", strings.Join(parts, " and ")+" in "+repo, grade
	if len(parts) == 0 {
		kind, title, body = "grade", grade+" in "+repo, ""
	}
	// The branch name comes from the indexed repository, so it is treated
	// like any other text taken from it.
	if cur.Branch != "" {
		title += " (" + redact.Free(clip(cur.Branch, notifyBranchMax)) + ")"
	}
	return Notification{ID: "findings:" + repo + "@" + cur.At, Kind: kind, Tone: "bad", Repo: repo,
		Title: clip(title, notifyTextMax), Body: body,
		Link: "/repos/" + url.PathEscape(repo) + "/security", At: at.UTC().Format(time.RFC3339)}, true
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// feedKey identifies what a feed may contain: the caller's organisations
// and visible repositories.
func feedKey(sc scope, repos []model.Node) string {
	var b strings.Builder
	if sc.all {
		b.WriteString("all|")
	}
	for _, m := range sc.orgs {
		b.WriteString(m.Org.ID + ",")
	}
	b.WriteString("|")
	for _, r := range repos {
		b.WriteString(r.Name)
		if reach, ok := sc.reach[r.Name]; ok {
			b.WriteString("@" + reach.OrgID)
		}
		b.WriteString(",")
	}
	return b.String()
}

func (c *notifyCache) get(key string) ([]Notification, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Since(e.at) > notifyCacheTTL {
		return nil, false
	}
	return e.items, true
}

func (c *notifyCache) put(key string, items []Notification) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil || len(c.entries) >= notifyCacheMax {
		c.entries = map[string]notifyCacheEntry{} // a full cache starts over
	}
	c.entries[key] = notifyCacheEntry{at: time.Now(), items: items}
}
