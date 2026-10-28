package api

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/license"
	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/reviews"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/usage"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

// navigationTools answer questions an agent would otherwise answer by
// reading files; their responses are compared with the measured file-reading
// baseline of the repository to estimate tokens avoided.
var navigationTools = map[string]bool{
	"get_symbol_context": true, "get_call_graph": true, "get_implementations": true, "get_pr_context": true,
	"minimal_context": true, "file_outline": true, "search": true, "search_all_repos": true, "get_config_entries": true,
	"get_doc_section": true, "repo_overview": true,
}

// recallTools read agent memory; recall_context is the one agents call first.
var recallTools = map[string]bool{"recall_context": true, "search_graph_nodes": true, "query_subgraph": true}

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
	Edits              EditStats  `json:"edits"`
	Tokens             TokenStats `json:"tokens"`
	ContextTotalTokens int        `json:"context_total_tokens"`
	ContextSavedPct    float64    `json:"context_saved_pct"`
	Sessions           int        `json:"sessions"`
	Calls              int        `json:"calls"`
	Errors             int        `json:"errors"`
	ResponseTokens     int        `json:"response_tokens"`
	// AvoidedTokens estimates tokens an agent would have read from files to
	// answer the same questions (see AvoidedMethod).
	AvoidedTokens int    `json:"avoided_tokens"`
	AvoidedMethod string `json:"avoided_method"`
	// LocalReadTokens is what local models read for ask_local; the part
	// the agent did not have to read is in AvoidedTokens.
	LocalReadTokens int `json:"local_read_tokens"`
	// LocalAvoidedTokens is the ask_local part of AvoidedTokens.
	LocalAvoidedTokens int `json:"local_avoided_tokens"`
	// LocalBriefs counts prompts a local model briefed the agent on.
	LocalBriefs int `json:"local_briefs"`
	// LocalDrafts counts drafts a local model wrote for an agent.
	LocalDrafts  int        `json:"local_drafts"`
	AvgLatencyMs float64    `json:"avg_latency_ms"`
	MaxLatencyMs int        `json:"max_latency_ms"`
	Daily        []DayStat  `json:"daily"`
	Tools        []ToolStat `json:"tools"`
	// Workflow says how sessions began, as the plugin's hooks reported.
	Workflow WorkflowStats `json:"workflow"`
	Clients  []ClientStat  `json:"clients"`
	Repos    []RepoUsage   `json:"repos"`
	// Unindexed lists checkouts agents worked in that the server has not
	// indexed, so they got no graph answers.
	Unindexed UnindexedStats `json:"unindexed"`
}

// UnindexedRepo is one checkout whose pre-finish check was skipped.
type UnindexedRepo struct {
	Repo    string `json:"repo"`
	Skipped int    `json:"skipped"`
}

// UnindexedStats recommends indexing: the checkouts without an index, most
// skipped first. AvoidedPerSession is what graph answers saved per session on
// the indexed repositories of the window, a reference and not a measurement
// of what these checkouts cost (0 when unknown).
type UnindexedStats struct {
	Repos             []UnindexedRepo `json:"repos"`
	Skipped           int             `json:"skipped"`
	AvoidedPerSession int             `json:"avoided_per_session"`
}

func unindexedStats(byRepo map[string]int, avoided, sessions int) UnindexedStats {
	u := UnindexedStats{Repos: []UnindexedRepo{}}
	for repo, n := range byRepo {
		u.Repos = append(u.Repos, UnindexedRepo{Repo: repo, Skipped: n})
		u.Skipped += n
	}
	slices.SortFunc(u.Repos, func(a, b UnindexedRepo) int {
		if c := cmp.Compare(b.Skipped, a.Skipped); c != 0 {
			return c
		}
		return cmp.Compare(a.Repo, b.Repo)
	})
	if sessions > 0 {
		u.AvoidedPerSession = avoided / sessions
	}
	return u
}

// TokenStats is the usage reported by agent hooks for instrumented turns.
// It is separate from MCP response-size and avoided-context estimates.
type TokenStats struct {
	usage.TokenCounts
	Total     int     `json:"total"`
	InputPct  float64 `json:"input_pct"`
	OutputPct float64 `json:"output_pct"`
}

// EditStats compares payload sizes with a hypothetical full-file rewrite.
type EditStats struct {
	usage.EditCounts
	EstimatedOutputSaved int     `json:"estimated_output_saved"`
	AvoidedPct           float64 `json:"avoided_pct"`
}

// WorkflowStats answer "do agents use CodeSynapse first?": of the sessions
// that looked at code, how many began with a CodeSynapse tool rather than
// Read, Grep or Glob, and how many whole-file reads the guard turned back.
// Only the plugin's hooks report them.
type WorkflowStats struct {
	FirstCodeSynapse int `json:"first_codesynapse"`
	FirstNative      int `json:"first_native"`
	ReadsTurnedBack  int `json:"reads_turned_back"`
	// FirstSharePct is FirstCodeSynapse of both, rounded; 0 with no sessions.
	FirstSharePct int `json:"first_share_pct"`
	// FilesEdited are the files sessions changed, FilesEditedFound those a
	// CodeSynapse answer of the session had pointed to, and EditRecallPct
	// their share (0 with no edits).
	FilesEdited      int `json:"files_edited"`
	FilesEditedFound int `json:"files_edited_found"`
	EditRecallPct    int `json:"edit_recall_pct"`
	// Bash counts, per class, shell commands a CodeSynapse tool answers with
	// less output (usage.BashClasses); BashTotal is their sum.
	Bash      map[string]int `json:"bash"`
	BashTotal int            `json:"bash_total"`
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
	Kind           string  `json:"kind"` // navigation, git, memory, review, prompt, other
	Calls          int     `json:"calls"`
	Errors         int     `json:"errors"`
	ResponseTokens int     `json:"response_tokens"`
	AvgTokens      float64 `json:"avg_tokens"`
	AvgMs          float64 `json:"avg_ms"`
	MaxMs          int     `json:"max_ms"`
	// RangesReturned and RangesRead say how many of the file line ranges
	// the tool's answers gave were then read (Claude Code plugin hooks).
	RangesReturned int `json:"ranges_returned"`
	RangesRead     int `json:"ranges_read"`
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
	Entities int `json:"entities"`
	Facts    int `json:"facts"`
	// History counts facts replaced by newer ones.
	History       int            `json:"history"`
	Confirmations int            `json:"confirmations"`
	Sources       map[string]int `json:"sources"`
	Repos         []memory.Stats `json:"repos"`
	Saves         int            `json:"saves"`
	Recalls       int            `json:"recalls"`
	RecallTokens  int            `json:"recall_tokens"`
	// RecallsByTool splits Recalls by the tool that read memory.
	RecallsByTool map[string]int `json:"recalls_by_tool"`
	// What the hooks did on their own: per-prompt recalls and the tokens
	// they added, conversations offloaded, their turns and the facts the
	// local model extracted from them.
	AutoRecalls      int           `json:"auto_recalls"`
	AutoRecallTokens int           `json:"auto_recall_tokens"`
	Offloads         int           `json:"offloads"`
	OffloadedTurns   int           `json:"offloaded_turns"`
	OffloadFacts     int           `json:"offload_facts"`
	Model            OffloadInputs `json:"model"`
	// Daily fills the whole window, empty days included; Clients ranks the
	// agents that saved and recalled.
	Daily   []MemoryDay    `json:"daily"`
	Clients []MemoryClient `json:"clients"`
}

// MemoryDay is one day of memory activity.
type MemoryDay struct {
	Day          string `json:"day"`
	Saves        int    `json:"saves"`
	Recalls      int    `json:"recalls"`
	AutoRecalls  int    `json:"auto_recalls"`
	Offloads     int    `json:"offloads"`
	OffloadFacts int    `json:"offload_facts"`
}

// MemoryClient is one agent's saves and recalls in the window.
type MemoryClient struct {
	Client  string `json:"client"`
	Saves   int    `json:"saves"`
	Recalls int    `json:"recalls"`
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

	// The sections read different parts of the graph; each also reads its
	// repositories a few at a time (forEach).
	sections := []func(context.Context) error{
		func(ctx context.Context) error {
			buckets, err := s.usageBuckets(ctx, days, now)
			if err != nil {
				return err
			}
			st.Agents = agentUsage(buckets, ov.Repos, days, now)
			st.Memory, err = s.memoryStats(ctx, buckets, st.Agents.Sessions, days, now)
			return err
		},
		func(ctx context.Context) (err error) { st.Hygiene, err = s.hygieneStats(ctx, ov.Repos); return err },
		func(ctx context.Context) (err error) { st.Reviews, err = s.reviewStats(ctx, ov.Repos); return err },
		func(ctx context.Context) (err error) { st.Licenses, err = s.licenseStats(ctx, ov.Repos); return err },
	}
	if err := forEach(ctx, len(sections), func(ctx context.Context, i int) error { return sections[i](ctx) }); err != nil {
		return st, err
	}
	if s.cfg.LLM != nil {
		for _, p := range s.cfg.LLM.Configs() {
			st.AI = append(st.AI, AIStat{Provider: p.Name, Model: p.Model, Local: p.Local(), Usage: s.cfg.LLM.Usage(p.Name)})
		}
	}
	return st, nil
}

// usageBuckets reads the MCP usage of the last days days that the caller's
// organisations may see.
func (s *Server) usageBuckets(ctx context.Context, days int, now time.Time) ([]usage.Bucket, error) {
	buckets, err := s.st.Usage(ctx, usage.Day(now.AddDate(0, 0, -(days-1))))
	if err != nil {
		return nil, err
	}
	return visibleUsage(scopeFrom(ctx), buckets), nil
}

// visibleUsage keeps the usage an organisation may see: its own agents'
// calls, even on a repository it shares with others. Calls that named no
// repository (and the sessions that connected) have none to check, so they
// belong to the organisation that made them.
func visibleUsage(sc scope, buckets []usage.Bucket) []usage.Bucket {
	if sc.all {
		return buckets
	}
	mine := map[string]bool{}
	for _, m := range sc.orgs {
		mine[m.Org.ID] = true
	}
	return slices.DeleteFunc(buckets, func(b usage.Bucket) bool {
		// A checkout the server does not index has no repository to check:
		// like a call without one, it belongs to the organisation that reported it.
		if b.Repo == "" || b.Tool == usage.Unindexed {
			return b.Org == "" || !mine[b.Org]
		}
		return !seesReview(sc, b.Repo, b.Org)
	})
}

// agentUsage compares what agents' calls returned with what reading the
// files would have cost, using each repository's benchmark ratio.
func agentUsage(buckets []usage.Bucket, repos []RepoSummary, days int, now time.Time) AgentStats {
	// Tokens a file-reading agent needs per graph token, per repository,
	// from the benchmark measured at ingest.
	ratio := map[string]float64{}
	var g, b int
	for _, rs := range repos {
		if rs.GraphTokens > 0 {
			ratio[rs.Name] = float64(rs.BaseTokens) / float64(rs.GraphTokens)
			g, b = g+rs.GraphTokens, b+rs.BaseTokens
		}
	}
	workspaceRatio := 1.0
	if g > 0 {
		workspaceRatio = float64(b) / float64(g)
	}
	return agentStats(buckets, days, now, ratio, workspaceRatio)
}

func agentStats(buckets []usage.Bucket, days int, now time.Time, ratio map[string]float64, workspaceRatio float64) AgentStats {
	a := AgentStats{Daily: []DayStat{}, Tools: []ToolStat{}, Clients: []ClientStat{}, Repos: []RepoUsage{},
		AvoidedMethod: "For successful calls of navigation tools (symbol context, call graph, outlines, search, ...), each response is compared with the tokens of the files an agent would read instead, using the ratio measured by that repository's token benchmark at ingest (workspace ratio when the call named no repository). For ask_local, the files a local model read in the agent's place, minus the answer the agent got. Tokens are bytes/4."}
	daily := map[string]*DayStat{}
	for i := days - 1; i >= 0; i-- {
		d := usage.Day(now.AddDate(0, 0, -i))
		daily[d] = &DayStat{Day: d}
		a.Daily = append(a.Daily, DayStat{Day: d})
	}
	tools := map[string]*ToolStat{}
	clients := map[string]*ClientStat{}
	repos := map[string]*RepoUsage{}
	unindexed := map[string]int{}
	var totalMs int
	for _, bk := range buckets {
		day := daily[bk.Day]
		if day == nil {
			continue
		}
		if a.Tokens.AddCounter(bk.Tool, bk.Calls) {
			continue
		}
		if a.Edits.AddCounter(bk.Tool, bk.Calls) {
			continue
		}
		if bk.Tool == usage.Unindexed {
			unindexed[bk.Repo] += bk.Calls
			continue
		}
		if bk.Tool == usage.LocalBriefs {
			a.LocalBriefs += bk.Calls
			continue
		}
		if bk.Tool == usage.LocalDrafts {
			a.LocalDrafts += bk.Calls
			continue
		}
		if bk.Tool == usage.LocalRead {
			a.LocalReadTokens += bk.Calls
			continue
		}
		if bk.Tool == usage.LocalAvoided {
			a.AvoidedTokens += bk.Calls
			a.LocalAvoidedTokens += bk.Calls
			day.AvoidedTokens += bk.Calls
			ru := repos[bk.Repo]
			if ru == nil {
				ru = &RepoUsage{Repo: bk.Repo}
				repos[bk.Repo] = ru
			}
			ru.AvoidedTokens += bk.Calls
			continue
		}
		if usage.IsWorkflow(bk.Tool) {
			if class, ok := usage.BashClassOf(bk.Tool); ok {
				if a.Workflow.Bash == nil {
					a.Workflow.Bash = map[string]int{}
				}
				a.Workflow.Bash[class] += bk.Calls
				a.Workflow.BashTotal += bk.Calls
				continue
			}
			switch bk.Tool {
			case usage.FirstCodeSynapse:
				a.Workflow.FirstCodeSynapse += bk.Calls
			case usage.FirstNative:
				a.Workflow.FirstNative += bk.Calls
			case usage.ReadTurnedBack:
				a.Workflow.ReadsTurnedBack += bk.Calls
			case usage.FilesEdited:
				a.Workflow.FilesEdited += bk.Calls
			case usage.FilesEditedFound:
				a.Workflow.FilesEditedFound += bk.Calls
			}
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
		t.RangesReturned += bk.RangesReturned
		t.RangesRead += bk.RangesRead
		ru := repos[bk.Repo]
		if ru == nil {
			ru = &RepoUsage{Repo: bk.Repo}
			repos[bk.Repo] = ru
		}
		ru.Calls += bk.Calls
		ru.ResponseTokens += bk.ResponseTokens
		ru.AvoidedTokens += avoided
	}
	a.Unindexed = unindexedStats(unindexed, a.AvoidedTokens, a.Sessions)
	a.Tokens.Total = a.Tokens.Input + a.Tokens.Output
	a.Edits.EstimatedOutputSaved = a.Edits.AvoidedBytes / 4
	if a.Edits.BaselineBytes > 0 {
		a.Edits.AvoidedPct = round1(100 * float64(a.Edits.AvoidedBytes) / float64(a.Edits.BaselineBytes))
	}
	if a.Tokens.Total > 0 {
		a.Tokens.InputPct = round1(100 * float64(a.Tokens.Input) / float64(a.Tokens.Total))
		a.Tokens.OutputPct = round1(100 - a.Tokens.InputPct)
	}
	a.ContextTotalTokens = a.ResponseTokens + a.AvoidedTokens
	if a.ContextTotalTokens > 0 {
		a.ContextSavedPct = round1(100 * float64(a.AvoidedTokens) / float64(a.ContextTotalTokens))
	}
	for i := range a.Daily {
		a.Daily[i] = *daily[a.Daily[i].Day]
	}
	if n := a.Workflow.FirstCodeSynapse + a.Workflow.FirstNative; n > 0 {
		a.Workflow.FirstSharePct = int(math.Round(100 * float64(a.Workflow.FirstCodeSynapse) / float64(n)))
	}
	if a.Workflow.FilesEdited > 0 {
		a.Workflow.EditRecallPct = int(math.Round(100 * float64(min(a.Workflow.FilesEditedFound, a.Workflow.FilesEdited)) / float64(a.Workflow.FilesEdited)))
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
	case recallTools[tool] || tool == "upsert_graph_triples" || tool == "offload_turns":
		return "memory"
	case strings.HasPrefix(tool, "git_"):
		return "git"
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

func (s *Server) memoryStats(ctx context.Context, buckets []usage.Bucket, sessions, days int, now time.Time) (MemoryStats, error) {
	m := MemoryStats{Sources: map[string]int{}, RecallsByTool: map[string]int{}, Repos: []memory.Stats{}, Daily: []MemoryDay{}, Clients: []MemoryClient{}}
	repos, err := s.st.MemoryStats(ctx)
	if err != nil {
		return m, err
	}
	chars := 0
	sc := scopeFrom(ctx)
	for _, r := range repos {
		// Namespaces are "org|graph" for organisations, the graph otherwise.
		if org, graph, ok := strings.Cut(r.Repo, "|"); ok {
			if !seesReview(sc, graph, org) {
				continue
			}
		} else if !sc.all {
			continue
		}
		m.Repos = append(m.Repos, r)
		m.Entities += r.Entities
		m.Facts += r.Facts
		m.History += r.History
		m.Confirmations += r.Confirmations
		chars += r.FactChars
		for src, n := range r.Sources {
			m.Sources[src] += n
		}
	}
	daily := map[string]*MemoryDay{}
	for i := days - 1; i >= 0; i-- {
		m.Daily = append(m.Daily, MemoryDay{Day: usage.Day(now.AddDate(0, 0, -i))})
	}
	for i := range m.Daily {
		daily[m.Daily[i].Day] = &m.Daily[i]
	}
	clients := map[string]*MemoryClient{}
	client := func(name string) *MemoryClient {
		if clients[name] == nil {
			clients[name] = &MemoryClient{Client: name}
		}
		return clients[name]
	}
	day := func(d string) *MemoryDay {
		if p := daily[d]; p != nil {
			return p
		}
		return &MemoryDay{} // outside the window: counted in the totals only
	}
	for _, b := range buckets {
		switch {
		case b.Tool == "upsert_graph_triples":
			m.Saves += b.Calls
			day(b.Day).Saves += b.Calls
			client(b.Client).Saves += b.Calls
		case recallTools[b.Tool]:
			m.Recalls += b.Calls
			m.RecallTokens += b.ResponseTokens
			m.RecallsByTool[b.Tool] += b.Calls
			day(b.Day).Recalls += b.Calls
			client(b.Client).Recalls += b.Calls
		case b.Tool == usage.MemoryAutoRecalls:
			m.AutoRecalls += b.Calls
			day(b.Day).AutoRecalls += b.Calls
		case b.Tool == usage.MemoryAutoRecallContext:
			m.AutoRecallTokens += b.Calls
		case b.Tool == usage.MemoryOffloads:
			m.Offloads += b.Calls
			day(b.Day).Offloads += b.Calls
		case b.Tool == usage.MemoryOffloadTurns:
			m.OffloadedTurns += b.Calls
		case b.Tool == usage.MemoryOffloadFacts:
			m.OffloadFacts += b.Calls
			day(b.Day).OffloadFacts += b.Calls
		}
	}
	for _, c := range clients {
		m.Clients = append(m.Clients, *c)
	}
	slices.SortFunc(m.Clients, func(x, y MemoryClient) int {
		return cmp.Or(cmp.Compare(y.Saves+y.Recalls, x.Saves+x.Recalls), cmp.Compare(x.Client, y.Client))
	})
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
	perRepo := make([]ruleHits, len(repos))
	if err := forEach(ctx, len(repos), func(ctx context.Context, i int) (err error) {
		perRepo[i], err = s.findingRules(ctx, repos[i])
		return err
	}); err != nil {
		return h, err
	}
	for i, rs := range repos {
		renovate := 0
		names := slices.Sorted(maps.Keys(perRepo[i]))
		for _, rule := range names {
			hit := perRepo[i][rule]
			if rule == "dependency-updates-missing" {
				h.NoUpdateAutomation = append(h.NoUpdateAutomation, rs.Name)
			}
			if strings.HasPrefix(rule, "renovate-") {
				renovate += hit.findings
			}
			rc := rules[rule]
			if rc == nil {
				rc = &RuleCount{Rule: rule, Severity: hit.severity}
				rules[rule] = rc
			}
			rc.Findings += hit.findings
			rc.Repos++
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
	lists := make([][]reviews.Review, len(repos))
	if err := forEach(ctx, len(repos), func(ctx context.Context, i int) (err error) {
		lists[i], err = s.st.ListReviews(ctx, repos[i].Name, store.MaxLimit)
		return err
	}); err != nil {
		return rv, err
	}
	for _, list := range lists {
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
