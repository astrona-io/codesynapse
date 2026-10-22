package api

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/license"
	"github.com/parisnakitakejser/codesynapse/internal/memory"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/store"
	"github.com/parisnakitakejser/codesynapse/internal/usage"
	"github.com/parisnakitakejser/codesynapse/services/api/internal/llm"
)

// navigationTools answer questions an agent would otherwise answer by
// reading files; their responses are compared with the measured file-reading
// baseline of the repository to estimate tokens avoided.
var navigationTools = map[string]bool{
	"get_symbol_context": true, "get_call_graph": true, "get_implementations": true, "get_pr_context": true,
	"minimal_context": true, "file_outline": true, "search": true, "search_all_repos": true, "get_config_entries": true,
	"get_doc_section": true, "repo_overview": true,
}

var recallTools = map[string]bool{"search_graph_nodes": true, "query_subgraph": true}

// Stats is GET /api/v1/stats: everything measured across the workspace.
type Stats struct {
	GeneratedAt string        `json:"generated_at"`
	Days        int           `json:"days"`
	Agents      AgentStats    `json:"agents"`
	Memory      MemoryStats   `json:"memory"`
	Licenses    LicenseStats  `json:"licenses"`
	Reviews     ReviewStats   `json:"reviews"`
	Hygiene     HygieneStats  `json:"hygiene"`
	AI          []AIStat      `json:"ai"`
	Workspace   Totals        `json:"workspace"`
	Jobs        JobHealth     `json:"jobs"`
	Repos       []RepoSummary `json:"repos"`
}

// AgentStats summarises MCP usage in the window.
type AgentStats struct {
	Sessions       int `json:"sessions"`
	Calls          int `json:"calls"`
	Errors         int `json:"errors"`
	ResponseTokens int `json:"response_tokens"`
	// AvoidedTokens estimates tokens an agent would have read from files to
	// answer the same questions (see AvoidedMethod).
	AvoidedTokens int          `json:"avoided_tokens"`
	AvoidedMethod string       `json:"avoided_method"`
	AvgLatencyMs  float64      `json:"avg_latency_ms"`
	MaxLatencyMs  int          `json:"max_latency_ms"`
	Daily         []DayStat    `json:"daily"`
	Tools         []ToolStat   `json:"tools"`
	Clients       []ClientStat `json:"clients"`
	Repos         []RepoUsage  `json:"repos"`
}

// DayStat is one day of usage.
type DayStat struct {
	Day            string `json:"day"`
	Sessions       int    `json:"sessions"`
	Calls          int    `json:"calls"`
	Errors         int    `json:"errors"`
	ResponseTokens int    `json:"response_tokens"`
	AvoidedTokens  int    `json:"avoided_tokens"`
}

// ToolStat is one tool (or "prompt:<name>") over the window.
type ToolStat struct {
	Tool           string  `json:"tool"`
	Kind           string  `json:"kind"` // navigation, memory, review, prompt, other
	Calls          int     `json:"calls"`
	Errors         int     `json:"errors"`
	ResponseTokens int     `json:"response_tokens"`
	AvgTokens      float64 `json:"avg_tokens"`
	AvgMs          float64 `json:"avg_ms"`
	MaxMs          int     `json:"max_ms"`
}

// ClientStat is one MCP client (Claude Code, Codex, Cursor, ...).
type ClientStat struct {
	Client   string `json:"client"`
	Sessions int    `json:"sessions"`
	Calls    int    `json:"calls"`
}

// RepoUsage is MCP usage of one repository ("" = calls without a repo
// argument, answered from the only or default repository).
type RepoUsage struct {
	Repo           string `json:"repo"`
	Calls          int    `json:"calls"`
	ResponseTokens int    `json:"response_tokens"`
	AvoidedTokens  int    `json:"avoided_tokens"`
}

// MemoryStats summarises agent memory and how it is used. Model holds the
// measured inputs of the offloading estimate the page computes.
type MemoryStats struct {
	Entities      int            `json:"entities"`
	Facts         int            `json:"facts"`
	Confirmations int            `json:"confirmations"`
	Sources       map[string]int `json:"sources"`
	Repos         []memory.Stats `json:"repos"`
	Saves         int            `json:"saves"`
	Recalls       int            `json:"recalls"`
	RecallTokens  int            `json:"recall_tokens"`
	Model         OffloadInputs  `json:"model"`
}

// OffloadInputs are the measured numbers behind the offloading estimate.
type OffloadInputs struct {
	// TokensPerFact is the recall cost of one fact: its text plus the JSON
	// around it, measured from recall responses when there are any.
	TokensPerFact float64 `json:"tokens_per_fact"`
	// TokensPerRecall is the average recall response.
	TokensPerRecall float64 `json:"tokens_per_recall"`
	// RecallsPerSession is recalls divided by sessions in the window.
	RecallsPerSession float64 `json:"recalls_per_session"`
	Measured          bool    `json:"measured"` // false: defaults, no recalls yet
}

// LicenseStats aggregates the licence reports of every repository.
type LicenseStats struct {
	Dependencies int             `json:"dependencies"`
	OK           int             `json:"ok"`
	Review       int             `json:"review"`
	Conflict     int             `json:"conflict"`
	Unknown      int             `json:"unknown"`
	Unlicensed   []string        `json:"unlicensed"`
	Conflicts    []RepoCount     `json:"conflicts"`
	ByLicense    []license.Count `json:"by_license"`
	Projects     map[string]int  `json:"projects"`
}

// RepoCount is a repository with a count.
type RepoCount struct {
	Repo  string `json:"repo"`
	Count int    `json:"count"`
}

// ReviewStats counts reviews and their comments.
type ReviewStats struct {
	Reviews  int            `json:"reviews"`
	Comments int            `json:"comments"`
	ByStatus map[string]int `json:"by_status"`
	ByKind   map[string]int `json:"by_kind"`
	BySource map[string]int `json:"by_source"`
	Verdicts map[string]int `json:"verdicts"`
	Fixes    map[string]int `json:"fixes"`
}

// HygieneStats are rule-level findings across the workspace.
type HygieneStats struct {
	NoUpdateAutomation []string    `json:"no_update_automation"`
	RenovateIssues     []RepoCount `json:"renovate_issues"`
	TopRules           []RuleCount `json:"top_rules"`
}

// RuleCount is how often a rule fired, and in how many repositories.
type RuleCount struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Findings int    `json:"findings"`
	Repos    int    `json:"repos"`
}

// AIStat is one configured model provider's usage since the API started.
type AIStat struct {
	Provider string    `json:"provider"`
	Model    string    `json:"model"`
	Local    bool      `json:"local"`
	Usage    llm.Usage `json:"usage"`
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days")) //nolint:errcheck // bad values fall back to 30
	if days <= 0 || days > 180 {
		days = 30
	}
	st, err := s.buildStats(r.Context(), days, time.Now())
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	writeJSON(w, 200, st)
}

func (s *Server) buildStats(ctx context.Context, days int, now time.Time) (Stats, error) {
	ov, err := s.buildOverview(ctx)
	if err != nil {
		return Stats{}, err
	}
	st := Stats{GeneratedAt: now.UTC().Format(time.RFC3339), Days: days, Workspace: ov.Totals, Jobs: ov.Jobs, Repos: ov.Repos, AI: []AIStat{}}

	// Tokens a file-reading agent needs per graph token, per repository,
	// from the benchmark measured at ingest.
	ratio := map[string]float64{}
	var g, b int
	for _, rs := range ov.Repos {
		if rs.GraphTokens > 0 {
			ratio[rs.Name] = float64(rs.BaseTokens) / float64(rs.GraphTokens)
			g, b = g+rs.GraphTokens, b+rs.BaseTokens
		}
	}
	workspaceRatio := 1.0
	if g > 0 {
		workspaceRatio = float64(b) / float64(g)
	}
	buckets, err := s.st.Usage(ctx, usage.Day(now.AddDate(0, 0, -(days-1))))
	if err != nil {
		return st, err
	}
	if sc := scopeFrom(ctx); !sc.all {
		buckets = slices.DeleteFunc(buckets, func(b usage.Bucket) bool { return !sc.sees(b.Repo) })
	}
	st.Agents = agentStats(buckets, days, now, ratio, workspaceRatio)

	if st.Memory, err = s.memoryStats(ctx, buckets, st.Agents.Sessions); err != nil {
		return st, err
	}
	if st.Hygiene, err = s.hygieneStats(ctx, ov.Repos); err != nil {
		return st, err
	}
	if st.Reviews, err = s.reviewStats(ctx, ov.Repos); err != nil {
		return st, err
	}
	if st.Licenses, err = s.licenseStats(ctx, ov.Repos); err != nil {
		return st, err
	}
	if s.cfg.LLM != nil {
		for _, p := range s.cfg.LLM.Configs() {
			st.AI = append(st.AI, AIStat{Provider: p.Name, Model: p.Model, Local: p.Local(), Usage: s.cfg.LLM.Usage(p.Name)})
		}
	}
	return st, nil
}

func agentStats(buckets []usage.Bucket, days int, now time.Time, ratio map[string]float64, workspaceRatio float64) AgentStats {
	a := AgentStats{Daily: []DayStat{}, Tools: []ToolStat{}, Clients: []ClientStat{}, Repos: []RepoUsage{},
		AvoidedMethod: "For successful calls of navigation tools (symbol context, call graph, outlines, search, ...), each response is compared with the tokens of the files an agent would read instead, using the ratio measured by that repository's token benchmark at ingest (workspace ratio when the call named no repository). Tokens are bytes/4."}
	daily := map[string]*DayStat{}
	for i := days - 1; i >= 0; i-- {
		d := usage.Day(now.AddDate(0, 0, -i))
		daily[d] = &DayStat{Day: d}
		a.Daily = append(a.Daily, DayStat{Day: d})
	}
	tools := map[string]*ToolStat{}
	clients := map[string]*ClientStat{}
	repos := map[string]*RepoUsage{}
	var totalMs int
	for _, bk := range buckets {
		day := daily[bk.Day]
		if day == nil {
			continue
		}
		cl := clients[bk.Client]
		if cl == nil {
			cl = &ClientStat{Client: bk.Client}
			clients[bk.Client] = cl
		}
		if bk.Tool == usage.Session {
			a.Sessions += bk.Calls
			day.Sessions += bk.Calls
			cl.Sessions += bk.Calls
			continue
		}
		avoided := 0
		if navigationTools[bk.Tool] && bk.Calls > 0 {
			r, known := ratio[bk.Repo]
			if !known {
				r = workspaceRatio
			}
			// Failed calls (ambiguous names, bad arguments) answered nothing,
			// so only the successful share of the tokens counts.
			ok := float64(bk.Calls-bk.Errors) / float64(bk.Calls)
			avoided = max(0, int(float64(bk.ResponseTokens)*ok*(r-1)))
		}
		a.Calls += bk.Calls
		a.Errors += bk.Errors
		a.ResponseTokens += bk.ResponseTokens
		a.AvoidedTokens += avoided
		a.MaxLatencyMs = max(a.MaxLatencyMs, bk.MaxDurationMs)
		totalMs += bk.DurationMs
		day.Calls += bk.Calls
		day.Errors += bk.Errors
		day.ResponseTokens += bk.ResponseTokens
		day.AvoidedTokens += avoided
		cl.Calls += bk.Calls
		t := tools[bk.Tool]
		if t == nil {
			t = &ToolStat{Tool: bk.Tool, Kind: toolKind(bk.Tool)}
			tools[bk.Tool] = t
		}
		t.Calls += bk.Calls
		t.Errors += bk.Errors
		t.ResponseTokens += bk.ResponseTokens
		t.AvgMs += float64(bk.DurationMs) // summed here, averaged below
		t.MaxMs = max(t.MaxMs, bk.MaxDurationMs)
		ru := repos[bk.Repo]
		if ru == nil {
			ru = &RepoUsage{Repo: bk.Repo}
			repos[bk.Repo] = ru
		}
		ru.Calls += bk.Calls
		ru.ResponseTokens += bk.ResponseTokens
		ru.AvoidedTokens += avoided
	}
	for i := range a.Daily {
		a.Daily[i] = *daily[a.Daily[i].Day]
	}
	if a.Calls > 0 {
		a.AvgLatencyMs = round1(float64(totalMs) / float64(a.Calls))
	}
	for _, t := range tools {
		if t.Calls > 0 {
			t.AvgTokens = round1(float64(t.ResponseTokens) / float64(t.Calls))
			t.AvgMs = round1(t.AvgMs / float64(t.Calls))
		}
		a.Tools = append(a.Tools, *t)
	}
	slices.SortFunc(a.Tools, func(x, y ToolStat) int { return cmp.Or(cmp.Compare(y.Calls, x.Calls), cmp.Compare(x.Tool, y.Tool)) })
	for _, c := range clients {
		a.Clients = append(a.Clients, *c)
	}
	slices.SortFunc(a.Clients, func(x, y ClientStat) int {
		return cmp.Or(cmp.Compare(y.Sessions+y.Calls, x.Sessions+x.Calls), cmp.Compare(x.Client, y.Client))
	})
	for _, r := range repos {
		a.Repos = append(a.Repos, *r)
	}
	slices.SortFunc(a.Repos, func(x, y RepoUsage) int { return cmp.Or(cmp.Compare(y.Calls, x.Calls), cmp.Compare(x.Repo, y.Repo)) })
	return a
}

func toolKind(tool string) string {
	switch {
	case strings.HasPrefix(tool, "prompt:"):
		return "prompt"
	case navigationTools[tool]:
		return "navigation"
	case recallTools[tool] || tool == "upsert_graph_triples":
		return "memory"
	case strings.Contains(tool, "review"):
		return "review"
	}
	return "other"
}

// Fallbacks for the offloading model before any recall was measured: a
// fact's text plus roughly 60 characters of JSON (ids, times, source).
const (
	factJSONChars        = 60
	defaultRecallsPerSes = 2
)

func (s *Server) memoryStats(ctx context.Context, buckets []usage.Bucket, sessions int) (MemoryStats, error) {
	m := MemoryStats{Sources: map[string]int{}, Repos: []memory.Stats{}}
	repos, err := s.st.MemoryStats(ctx)
	if err != nil {
		return m, err
	}
	chars := 0
	sc := scopeFrom(ctx)
	for _, r := range repos {
		if !sc.sees(r.Repo) {
			continue
		}
		m.Repos = append(m.Repos, r)
		m.Entities += r.Entities
		m.Facts += r.Facts
		m.Confirmations += r.Confirmations
		chars += r.FactChars
		for src, n := range r.Sources {
			m.Sources[src] += n
		}
	}
	for _, b := range buckets {
		switch {
		case b.Tool == "upsert_graph_triples":
			m.Saves += b.Calls
		case recallTools[b.Tool]:
			m.Recalls += b.Calls
			m.RecallTokens += b.ResponseTokens
		}
	}
	in := OffloadInputs{TokensPerFact: 20, RecallsPerSession: defaultRecallsPerSes}
	if m.Facts > 0 {
		in.TokensPerFact = round1(float64(chars/m.Facts+factJSONChars) / 4)
	}
	if m.Recalls > 0 {
		in.TokensPerRecall = round1(float64(m.RecallTokens) / float64(m.Recalls))
		in.Measured = true
		if sessions > 0 {
			in.RecallsPerSession = round1(float64(m.Recalls) / float64(sessions))
		}
	} else {
		in.TokensPerRecall = round1(in.TokensPerFact * 10) // a typical recall returns about ten facts
	}
	m.Model = in
	return m, nil
}

func (s *Server) hygieneStats(ctx context.Context, repos []RepoSummary) (HygieneStats, error) {
	h := HygieneStats{NoUpdateAutomation: []string{}, RenovateIssues: []RepoCount{}, TopRules: []RuleCount{}}
	rules := map[string]*RuleCount{}
	for _, rs := range repos {
		findings, err := s.st.List(ctx, rs.Name, store.Filter{Kinds: []model.NodeKind{model.KindFinding}}, store.ByName, store.MaxLimit)
		if err != nil {
			return h, err
		}
		seen := map[string]bool{}
		renovate := 0
		for _, f := range findings {
			rule := f.Node.Rule
			if rule == "dependency-updates-missing" && !seen[rule] {
				h.NoUpdateAutomation = append(h.NoUpdateAutomation, rs.Name)
			}
			if strings.HasPrefix(rule, "renovate-") {
				renovate++
			}
			rc := rules[rule]
			if rc == nil {
				rc = &RuleCount{Rule: rule, Severity: f.Node.Severity}
				rules[rule] = rc
			}
			rc.Findings++
			if !seen[rule] {
				rc.Repos++
				seen[rule] = true
			}
		}
		if renovate > 0 {
			h.RenovateIssues = append(h.RenovateIssues, RepoCount{rs.Name, renovate})
		}
	}
	for _, rc := range rules {
		h.TopRules = append(h.TopRules, *rc)
	}
	slices.SortFunc(h.TopRules, func(x, y RuleCount) int {
		return cmp.Or(cmp.Compare(y.Repos, x.Repos), cmp.Compare(y.Findings, x.Findings), cmp.Compare(x.Rule, y.Rule))
	})
	h.TopRules = h.TopRules[:min(len(h.TopRules), 15)]
	return h, nil
}

func (s *Server) reviewStats(ctx context.Context, repos []RepoSummary) (ReviewStats, error) {
	rv := ReviewStats{ByStatus: map[string]int{}, ByKind: map[string]int{}, BySource: map[string]int{}, Verdicts: map[string]int{}, Fixes: map[string]int{}}
	for _, rs := range repos {
		list, err := s.st.ListReviews(ctx, rs.Name, store.MaxLimit)
		if err != nil {
			return rv, err
		}
		for _, r := range list {
			rv.Reviews++
			rv.ByKind[cmp.Or(r.Kind, "review")]++
			rv.BySource[cmp.Or(r.Source, "unknown")]++
			if r.Verdict != "" {
				rv.Verdicts[r.Verdict]++
			}
			for _, c := range r.Comments {
				rv.Comments++
				rv.ByStatus[c.Status]++
				if c.Fix != nil {
					rv.Fixes[c.Fix.State]++
				}
			}
		}
	}
	return rv, nil
}

func (s *Server) licenseStats(ctx context.Context, repos []RepoSummary) (LicenseStats, error) {
	l := LicenseStats{Unlicensed: []string{}, Conflicts: []RepoCount{}, ByLicense: []license.Count{}, Projects: map[string]int{}}
	counts := map[string]*license.Count{}
	for _, rs := range repos {
		n, err := s.st.Node(ctx, model.NodeID(rs.Name, model.KindRepository, rs.Name))
		if err != nil {
			return l, err
		}
		if n.Licenses == "" {
			continue // indexed before licence checks existed
		}
		var rep license.Report
		if err := json.Unmarshal([]byte(n.Licenses), &rep); err != nil {
			continue
		}
		l.Dependencies += rep.Total
		l.OK += rep.OK
		l.Review += rep.Review
		l.Conflict += rep.Conflict
		l.Unknown += rep.Unknown
		l.Projects[cmp.Or(rep.Project.License, "none")]++
		if rep.Project.License == "" {
			l.Unlicensed = append(l.Unlicensed, rs.Name)
		}
		if rep.Conflict > 0 {
			l.Conflicts = append(l.Conflicts, RepoCount{rs.Name, rep.Conflict})
		}
		for _, c := range rep.ByLicense {
			if counts[c.License] == nil {
				counts[c.License] = &license.Count{License: c.License, Category: c.Category}
			}
			counts[c.License].Count += c.Count
		}
	}
	for _, c := range counts {
		l.ByLicense = append(l.ByLicense, *c)
	}
	slices.SortFunc(l.ByLicense, func(x, y license.Count) int {
		return cmp.Or(cmp.Compare(y.Count, x.Count), cmp.Compare(x.License, y.License))
	})
	slices.SortFunc(l.Conflicts, func(x, y RepoCount) int { return cmp.Or(cmp.Compare(y.Count, x.Count), cmp.Compare(x.Repo, y.Repo)) })
	return l, nil
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }
