package api

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/history"
	"github.com/astrona-io/codesynapse/internal/security"
)

// What needs attention, worst first, in words a team lead understands:
// the Overview page of the web app. Everything comes from data the other
// endpoints already compute; this ranks it and says what to do.

// AttentionItem is one thing to look at.
type AttentionItem struct {
	Level  string `json:"level"` // critical, high, medium, low
	Kind   string `json:"kind"`  // security, vulnerability, licence, access, sync, stale, automation, quality, regression
	Repo   string `json:"repo,omitempty"`
	Count  int    `json:"count,omitempty"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Action string `json:"action"`
	Link   string `json:"link"` // a path in the web app
}

// Attention is GET /api/v1/attention.
type Attention struct {
	GeneratedAt string          `json:"generated_at"`
	Items       []AttentionItem `json:"items"`
	Levels      map[string]int  `json:"levels"`
	// Repos is how many repositories were checked; Clean those with
	// nothing to report.
	Repos int      `json:"repos"`
	Clean []string `json:"clean"`
	// Headline figures for the page.
	TokensSaved int            `json:"tokens_saved"`
	Findings    int            `json:"findings"`
	Vulns       int            `json:"vulns"`
	Grades      map[string]int `json:"grades"`
}

// A repository is flagged for duplicate code from this share of its function
// lines, and only when that is a meaningful amount; the cross-repository
// recommendations are capped so they do not crowd out anything urgent.
const (
	dupAttentionPct    = 5.0
	dupAttentionLOC    = 100
	maxSharedAttention = 3
)

var levelRank = map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3}
var kindRank = map[string]int{"access": 0, "vulnerability": 1, "security": 2, "licence": 3, "sync": 4, "stale": 5, "automation": 6, "quality": 7, "regression": 1}

func repoLink(repo, page string) string {
	p := "/repos/" + url.PathEscape(repo)
	if page != "" {
		p += "/" + page
	}
	return p
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func (s *Server) handleAttention(w http.ResponseWriter, r *http.Request) {
	a, err := s.buildAttention(r.Context(), r.URL.Query().Get("repo"), time.Now())
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	writeJSON(w, 200, a)
}

// buildAttention covers every visible repository, or only that one.
func (s *Server) buildAttention(ctx context.Context, only string, now time.Time) (Attention, error) {
	ov, err := s.overviewOf(ctx, only)
	if err != nil {
		return Attention{}, err
	}
	a := Attention{GeneratedAt: now.UTC().Format(time.RFC3339), Items: []AttentionItem{}, Levels: map[string]int{}, Clean: []string{},
		Repos: len(ov.Repos), TokensSaved: ov.Totals.TokensSaved, Grades: ov.Totals.Grades}
	add := func(it AttentionItem) { a.Items = append(a.Items, it) }

	for _, rs := range ov.Repos {
		for _, n := range rs.Findings {
			a.Findings += n
		}
		for _, n := range rs.Vulns {
			a.Vulns += n
		}
		if n := rs.VulnsCalled; n > 0 {
			add(AttentionItem{Level: "critical", Kind: "vulnerability", Repo: rs.Name, Count: n,
				Title:  plural(n, "vulnerable dependency your code calls", "vulnerable dependencies your code calls"),
				Detail: "Your code calls the affected functions, so these known flaws (CVEs) are likely exploitable. Updating the dependency usually fixes it.",
				Action: "See the dependencies", Link: repoLink(rs.Name, "dependencies")})
		}
		if n := rs.Findings["critical"]; n > 0 {
			add(AttentionItem{Level: "critical", Kind: "security", Repo: rs.Name, Count: n,
				Title:  plural(n, "critical security finding", "critical security findings"),
				Detail: "Problems such as commands or queries built from outside input, or secrets in configuration. Each finding explains the fix.",
				Action: "Review the findings", Link: repoLink(rs.Name, "security")})
		}
		if n := rs.Findings["high"]; n > 0 {
			add(AttentionItem{Level: "high", Kind: "security", Repo: rs.Name, Count: n,
				Title:  plural(n, "high-severity security finding", "high-severity security findings"),
				Detail: "Likely weaknesses worth fixing soon; findings reachable from entry points come first on the page.",
				Action: "Review the findings", Link: repoLink(rs.Name, "security")})
		}
		other := 0
		for _, n := range rs.Vulns {
			other += n
		}
		if other -= rs.VulnsCalled; other > 0 {
			add(AttentionItem{Level: "medium", Kind: "vulnerability", Repo: rs.Name, Count: other,
				Title:  plural(other, "dependency with a known vulnerability", "dependencies with known vulnerabilities"),
				Detail: "Your code does not call the affected functions as far as CodeSynapse can see, so the risk is lower; update when convenient.",
				Action: "See the dependencies", Link: repoLink(rs.Name, "dependencies")})
		}
		if rs.Grade == security.NotApplicable {
			// Not checked is not clean: say so instead of listing it as all clear.
			add(AttentionItem{Level: "low", Kind: "security", Repo: rs.Name,
				Title:  "Not covered by the security checks",
				Detail: "None of its files are in a language or format CodeSynapse's rules cover yet, so there is no grade. The Security page lists what was and was not checked; review the rest by hand.",
				Action: "See what was checked", Link: repoLink(rs.Name, "security")})
		}
		s.regressionItem(ctx, rs.Name, add)
		if rs.DupPct >= dupAttentionPct && rs.DupLOC >= dupAttentionLOC {
			add(AttentionItem{Level: "low", Kind: "quality", Repo: rs.Name, Count: rs.DupLOC,
				Title:  fmt.Sprintf("%d lines repeat code that already exists (%.0f%% of function code)", rs.DupLOC, rs.DupPct),
				Detail: "Copies have to be read, tested and fixed separately and drift apart. The page groups them and says whether to extract a helper or move them to a shared package.",
				Action: "See the duplicates", Link: repoLink(rs.Name, "duplicates")})
		}
		if rs.Stale {
			days := int(rs.AgeHours / 24)
			add(AttentionItem{Level: "low", Kind: "stale", Repo: rs.Name,
				Title:  fmt.Sprintf("Not indexed for %s", plural(days, "day", "days")),
				Detail: "Reports describe the code as it was then. Sync it, or turn on a schedule so it stays current.",
				Action: "Sync now", Link: repoLink(rs.Name, "")})
		}
	}

	lic, err := s.licenseStats(ctx, ov.Repos)
	if err != nil {
		return a, err
	}
	for _, c := range lic.Conflicts {
		add(AttentionItem{Level: "high", Kind: "licence", Repo: c.Repo, Count: c.Count,
			Title:  plural(c.Count, "dependency whose licence conflicts with yours", "dependencies whose licences conflict with yours"),
			Detail: "Using them may oblige you to publish your own code or stop distributing it. Replace them, or check with legal.",
			Action: "See the licences", Link: repoLink(c.Repo, "licenses")})
	}
	hy, err := s.hygieneStats(ctx, ov.Repos)
	if err != nil {
		return a, err
	}
	for _, repo := range hy.NoUpdateAutomation {
		add(AttentionItem{Level: "low", Kind: "automation", Repo: repo,
			Title:  "No automatic dependency updates",
			Detail: "Without Renovate or Dependabot, security fixes in dependencies arrive only when someone remembers. Adding a config takes minutes.",
			Action: "See the dependencies", Link: repoLink(repo, "dependencies")})
	}
	// A repository whose latest index or sync run failed (a later success
	// clears it). AI tasks are not counted.
	latest := map[string]Job{}
	for _, j := range s.jobs.List(ctx) {
		if j.Kind == "ai" || j.Kind == "fix" {
			continue // tasks, not indexing
		}
		if cur, ok := latest[j.Repo]; !ok || j.StartedAt > cur.StartedAt {
			latest[j.Repo] = j
		}
	}
	for repo, j := range latest {
		if j.State != JobFailed || !seesReview(scopeFrom(ctx), repo, j.OrgID) || (only != "" && repo != only) {
			continue
		}
		add(AttentionItem{Level: "high", Kind: "sync", Repo: repo,
			Title: "The last sync failed", Detail: firstLineOf(j.Error),
			Action: "Open the repository", Link: repoLink(repo, "")})
	}
	lost, err := s.lostAccess(ctx)
	if err != nil {
		return a, err
	}
	for _, l := range lost {
		if only != "" && l.Graph != only {
			continue
		}
		add(AttentionItem{Level: "high", Kind: "access", Repo: l.Graph,
			Title:  "Your organisation can no longer read this private repository",
			Detail: "Its Git token was removed or lost access, so the repository is hidden from your organisation. An admin can save a working token.",
			Action: "Fix the Git token", Link: "/settings/git-tokens"})
	}

	if only == "" {
		shared, err := s.sharedCode(ctx)
		if err != nil {
			return a, err
		}
		for _, sg := range shared.Suggestions[:min(len(shared.Suggestions), maxSharedAttention)] {
			add(AttentionItem{Level: "low", Kind: "quality", Count: sg.RedundantLOC,
				Title:  fmt.Sprintf("%d of your repositories copy the same code: share it as a %s", len(sg.Repos), sg.Kind),
				Detail: sg.Rationale,
				Action: "See the shared code", Link: "/shared-code"})
		}
	}

	slices.SortStableFunc(a.Items, func(x, y AttentionItem) int {
		return cmp.Or(cmp.Compare(levelRank[x.Level], levelRank[y.Level]), cmp.Compare(kindRank[x.Kind], kindRank[y.Kind]),
			cmp.Compare(y.Count, x.Count), cmp.Compare(x.Repo, y.Repo))
	})
	flagged := map[string]bool{}
	for _, it := range a.Items {
		a.Levels[it.Level]++
		flagged[it.Repo] = true
	}
	for _, rs := range ov.Repos {
		if !flagged[rs.Name] {
			a.Clean = append(a.Clean, rs.Name)
		}
	}
	return a, nil
}

// lostAccess lists the private entries of the request's organisations that
// lost access. They are outside the scope (hidden), so they are read from
// the registry directly, for the organisations the caller acts in.
func (s *Server) lostAccess(ctx context.Context) ([]accounts.Repository, error) {
	if s.cfg.Accounts == nil {
		return nil, nil
	}
	sc := scopeFrom(ctx)
	var orgIDs []string
	for _, m := range sc.orgs {
		if m.Role.AtLeast(accounts.Viewer) {
			orgIDs = append(orgIDs, m.Org.ID)
		}
	}
	var out []accounts.Repository
	for _, id := range orgIDs {
		repos, err := s.cfg.Accounts.St.OrgRepositories(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, r := range repos {
			if !r.Usable() {
				out = append(out, r)
			}
		}
	}
	return out, nil
}

func firstLineOf(s string) string {
	for i, c := range s {
		if c == '\n' {
			s = s[:i]
			break
		}
	}
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return s
}

// urgentRegressions are the changes that raise an item to high: a worse
// grade, more vulnerabilities your code calls, more serious findings, new
// licence conflicts. Anything else that got worse is a low item.
var urgentRegressions = map[string]bool{"grade": true, "vulns_called": true, "findings_high": true, "vulns_high": true, "licence_conflicts": true}

// regressionItem adds one item for a repository that got worse since the
// snapshot before the last ingest. One snapshot or none means nothing to say.
func (s *Server) regressionItem(ctx context.Context, repo string, add func(AttentionItem)) {
	snaps, err := s.st.Snapshots(ctx, repo, 2)
	if err != nil || len(snaps) < 2 {
		return
	}
	reg, _ := history.Compare(snaps[0], snaps[1])
	if len(reg) == 0 {
		return
	}
	level, details := "low", make([]string, 0, len(reg))
	for _, c := range reg {
		if urgentRegressions[c.Key] {
			level = "high"
		}
		details = append(details, c.Detail)
	}
	add(AttentionItem{Level: level, Kind: "regression", Repo: repo, Count: len(reg),
		Title:  "Got worse since the previous sync: " + strings.Join(details[:min(len(details), 3)], "; "),
		Detail: strings.Join(details, ". ") + ". Compared with the sync before the last one; the history page shows the trend.",
		Action: "See the trend", Link: repoLink(repo, "history")})
}
