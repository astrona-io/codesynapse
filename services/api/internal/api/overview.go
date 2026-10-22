package api

import (
	"cmp"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"slices"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/cleanup"
	"github.com/parisnakitakejser/codesynapse/internal/metrics"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/security"
)

// staleAfter marks repositories whose index is older than this.
const staleAfter = 7 * 24 * time.Hour

// RepoSummary is one row of the operator overview.
type RepoSummary struct {
	Name          string         `json:"name"`
	Branch        string         `json:"branch,omitempty"`
	Head          string         `json:"head,omitempty"`
	Remote        string         `json:"remote,omitempty"`
	Managed       bool           `json:"managed"`
	IngestedAt    string         `json:"ingested_at,omitempty"`
	AgeHours      float64        `json:"age_hours"`
	Stale         bool           `json:"stale"`
	Nodes         int            `json:"nodes"`
	Edges         int            `json:"edges"`
	LOC           int            `json:"loc"`
	Files         int            `json:"files"`
	Docs          int            `json:"docs"`
	Configs       int            `json:"configs"`
	Templates     int            `json:"templates"`
	Modules       int            `json:"modules"`
	Grade         string         `json:"grade"`
	Score         int            `json:"score"`
	Findings      map[string]int `json:"findings"`
	Vulns         map[string]int `json:"vulns"`
	VulnsCalled   int            `json:"vulns_called"`
	TestReachPct  float64        `json:"test_reach_pct"`
	DocCoverPct   float64        `json:"doc_coverage_pct"`
	AvgComplexity float64        `json:"avg_complexity"`
	ReductionPct  float64        `json:"reduction_pct"`
	Formats       map[string]int `json:"formats"`
	Schedule      Schedule       `json:"schedule"`
	K8sResources  int            `json:"k8s_resources"`
	HelmCharts    int            `json:"helm_charts"`
	DeadFuncs     int            `json:"dead_functions"`
	DeadLOC       int            `json:"dead_loc"`
	UnusedMods    int            `json:"unused_modules"`
	GraphTokens   int            `json:"-"`
	BaseTokens    int            `json:"-"`
}

// JobHealth summarises recent ingest jobs.
type JobHealth struct {
	Total          int     `json:"total"`
	Running        int     `json:"running"`
	Succeeded      int     `json:"succeeded"`
	Failed         int     `json:"failed"`
	AvgDurationSec float64 `json:"avg_duration_sec"`
	MaxDurationSec float64 `json:"max_duration_sec"`
	RecentFailures []Job   `json:"recent_failures"`
}

// Totals aggregate every repository.
type Totals struct {
	Repos         int                    `json:"repos"`
	Nodes         int                    `json:"nodes"`
	Edges         int                    `json:"edges"`
	LOC           int                    `json:"loc"`
	NodeKinds     map[model.NodeKind]int `json:"node_kinds"`
	EdgeKinds     map[model.EdgeKind]int `json:"edge_kinds"`
	Findings      map[string]int         `json:"findings"`
	Vulns         map[string]int         `json:"vulns"`
	VulnsCalled   int                    `json:"vulns_called"`
	Grades        map[string]int         `json:"grades"`
	Stale         int                    `json:"stale"`
	ReductionPct  float64                `json:"reduction_pct"`
	TokensSaved   int                    `json:"tokens_saved"`
	SecurityTags  map[string]int         `json:"security_tags"`
	VulnerableMod int                    `json:"vulnerable_modules"`
	DeadLOC       int                    `json:"dead_loc"`
	UnusedMods    int                    `json:"unused_modules"`
}

// Overview is GET /api/v1/overview.
type Overview struct {
	GeneratedAt string        `json:"generated_at"`
	Totals      Totals        `json:"totals"`
	Repos       []RepoSummary `json:"repos"`
	Jobs        JobHealth     `json:"jobs"`
}

func (s *Server) handleWorkspaceOverview(w http.ResponseWriter, r *http.Request) {
	ov, err := s.buildOverview(r.Context())
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	writeJSON(w, 200, ov)
}

// buildOverview summarises every repository; the statistics page reuses it.
func (s *Server) buildOverview(ctx context.Context) (Overview, error) {
	return s.overviewOf(ctx, "")
}

// overviewOf builds the overview of every visible repository, or of only
// that one.
func (s *Server) overviewOf(ctx context.Context, only string) (Overview, error) {
	repos, err := s.st.Repos(ctx)
	if err != nil {
		return Overview{}, err
	}
	repos = visibleRepos(ctx, repos)
	if only != "" {
		repos = slices.DeleteFunc(repos, func(n model.Node) bool { return n.Name != only })
	}
	now := time.Now()
	ov := Overview{
		GeneratedAt: now.UTC().Format(time.RFC3339),
		Repos:       []RepoSummary{},
		Totals: Totals{
			NodeKinds: map[model.NodeKind]int{}, EdgeKinds: map[model.EdgeKind]int{}, Findings: map[string]int{},
			Vulns: map[string]int{}, Grades: map[string]int{}, SecurityTags: map[string]int{},
		},
	}
	var graphTok, baseTok int
	for _, repoNode := range repos {
		name := repoNode.Name
		st, err := s.st.Stats(ctx, name)
		if err != nil {
			return Overview{}, err
		}
		sec, err := security.Build(ctx, s.st, name, security.Options{})
		if err != nil {
			return Overview{}, err
		}
		rs := RepoSummary{
			Name: name, Branch: repoNode.Branch, Head: repoNode.Head, Remote: repoNode.URL,
			Managed: s.inWorkspace(repoNode.Root), IngestedAt: repoNode.Time,
			LOC: st.TotalLOC, Files: st.Nodes[model.KindFile], Docs: st.Nodes[model.KindDocument],
			Configs: st.Nodes[model.KindConfig], Templates: st.Nodes[model.KindTemplate], Modules: sec.Modules.Total,
			Grade: sec.Grade, Score: sec.Score, Findings: sec.FindingCounts, Vulns: sec.VulnCounts,
			AvgComplexity: math.Round(st.AvgComplexity*10) / 10,
			Formats:       st.Formats, K8sResources: st.K8sResources, HelmCharts: st.HelmCharts,
			Schedule: s.sched.get(name),
		}
		for k, c := range st.Nodes {
			rs.Nodes += c
			ov.Totals.NodeKinds[k] += c
		}
		for k, c := range st.Edges {
			rs.Edges += c
			ov.Totals.EdgeKinds[k] += c
		}
		for t, c := range st.SecurityTags {
			ov.Totals.SecurityTags[t] += c
		}
		for _, v := range sec.Vulnerabilities {
			if v.Reachability == "called" {
				rs.VulnsCalled++
			}
		}
		if t, err := time.Parse(time.RFC3339, repoNode.Time); err == nil {
			age := now.Sub(t)
			rs.AgeHours = math.Round(age.Hours()*10) / 10
			rs.Stale = age > staleAfter
		}
		if repoNode.Cleanup != "" {
			var c cleanup.Report
			if json.Unmarshal([]byte(repoNode.Cleanup), &c) == nil {
				rs.DeadFuncs = c.Summary.Unreachable + c.Summary.TestOnly
				rs.DeadLOC, rs.UnusedMods = c.Summary.DeadLOC, c.Summary.UnusedModules
				ov.Totals.DeadLOC += c.Summary.DeadLOC
				ov.Totals.UnusedMods += c.Summary.UnusedModules
			}
		}
		if repoNode.Metrics != "" {
			var m metrics.Report
			if json.Unmarshal([]byte(repoNode.Metrics), &m) == nil {
				rs.TestReachPct, rs.DocCoverPct = m.Tests.ReachablePct, m.Docs.CoveragePct
				rs.ReductionPct = m.Benchmark.MedianReductionPct
				rs.GraphTokens, rs.BaseTokens = m.Benchmark.TotalGraph, m.Benchmark.TotalBaseline
				graphTok += m.Benchmark.TotalGraph
				baseTok += m.Benchmark.TotalBaseline
			}
		}

		ov.Totals.Repos++
		ov.Totals.Nodes += rs.Nodes
		ov.Totals.Edges += rs.Edges
		ov.Totals.LOC += rs.LOC
		ov.Totals.Grades[rs.Grade]++
		ov.Totals.VulnsCalled += rs.VulnsCalled
		ov.Totals.VulnerableMod += sec.Modules.Vulnerable
		for sev, c := range rs.Findings {
			ov.Totals.Findings[sev] += c
		}
		for sev, c := range rs.Vulns {
			ov.Totals.Vulns[sev] += c
		}
		if rs.Stale {
			ov.Totals.Stale++
		}
		ov.Repos = append(ov.Repos, rs)
	}
	if baseTok > 0 {
		ov.Totals.ReductionPct = math.Round((1-float64(graphTok)/float64(baseTok))*1000) / 10
		ov.Totals.TokensSaved = baseTok - graphTok
	}
	// Worst security posture first: that is what an operator acts on.
	slices.SortFunc(ov.Repos, func(a, b RepoSummary) int { return cmp.Or(cmp.Compare(a.Score, b.Score), cmp.Compare(a.Name, b.Name)) })
	ov.Jobs = jobHealth(s.jobs.List())
	return ov, nil
}

func jobHealth(jobs []Job) JobHealth {
	h := JobHealth{Total: len(jobs), RecentFailures: []Job{}}
	var sum float64
	var finished int
	for _, j := range jobs {
		switch j.State {
		case JobRunning:
			h.Running++
		case JobSucceeded:
			h.Succeeded++
		case JobFailed:
			h.Failed++
			if len(h.RecentFailures) < 5 {
				h.RecentFailures = append(h.RecentFailures, j)
			}
		}
		start, err1 := time.Parse(time.RFC3339, j.StartedAt)
		end, err2 := time.Parse(time.RFC3339, j.FinishedAt)
		if err1 == nil && err2 == nil {
			d := end.Sub(start).Seconds()
			sum += d
			finished++
			h.MaxDurationSec = math.Max(h.MaxDurationSec, d)
		}
	}
	if finished > 0 {
		h.AvgDurationSec = math.Round(sum/float64(finished)*10) / 10
	}
	return h
}
