// Package security assembles the security review of a repository from the
// graph: code findings from the rule engine and known vulnerabilities in
// dependencies, each with severity, CWE, location, exposure (reachable from
// an entry point), test reachability and a priority that combines them.
package security

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/query"
	"github.com/parisnakitakejser/codesynapse/internal/store"
)

// exposureDepth bounds the caller search for entry points.
const exposureDepth = 5

var severityWeight = map[string]int{"critical": 25, "high": 10, "medium": 3, "low": 1, "info": 0}

var severityBase = map[string]int{"critical": 90, "high": 70, "medium": 45, "low": 20, "info": 5}

// Finding is a code finding with context.
type Finding struct {
	ID       string     `json:"id"`
	Rule     string     `json:"rule"`
	Title    string     `json:"title"`
	Severity string     `json:"severity"`
	CWE      []string   `json:"cwe,omitempty"`
	Location string     `json:"location"`
	Language string     `json:"language,omitempty"`
	Detail   string     `json:"detail"`
	Owner    *query.Ref `json:"owner,omitempty"`
	// Exposed means the owner is an entry point or reachable from one
	// (main, an exported function, or an HTTP/MCP handler) within a few hops.
	Exposed bool `json:"exposed"`
	// Tested means a test function reaches the owner.
	Tested   bool `json:"tested"`
	Priority int  `json:"priority"`
}

// Vulnerability is a known vulnerability in a dependency.
type Vulnerability struct {
	ID           string      `json:"id"`
	CVE          []string    `json:"cve,omitempty"`
	Aliases      []string    `json:"aliases,omitempty"`
	Summary      string      `json:"summary"`
	Severity     string      `json:"severity"`
	CWE          []string    `json:"cwe,omitempty"`
	Module       string      `json:"module"`
	Version      string      `json:"version"`
	Fixed        string      `json:"fixed,omitempty"`
	Reachability string      `json:"reachability"`
	URL          string      `json:"url"`
	CallSites    []query.Ref `json:"call_sites,omitempty"`
	Priority     int         `json:"priority"`
}

// Report is the security review.
type Report struct {
	Repo            string          `json:"repo"`
	Score           int             `json:"score"`
	Grade           string          `json:"grade"`
	FindingCounts   map[string]int  `json:"finding_counts"`
	VulnCounts      map[string]int  `json:"vuln_counts"`
	Findings        []Finding       `json:"findings"`
	Vulnerabilities []Vulnerability `json:"vulnerabilities"`
	Modules         ModuleStats     `json:"modules"`
	Rules           []RuleCount     `json:"rules"`
	Sensitive       []query.Ref     `json:"sensitive_functions"`
}

// ModuleStats counts dependencies.
type ModuleStats struct {
	Total      int `json:"total"`
	Direct     int `json:"direct"`
	Indirect   int `json:"indirect"`
	Vulnerable int `json:"vulnerable"`
	// Stdlib is the Go version checked for standard library vulnerabilities.
	Stdlib string `json:"stdlib,omitempty"`
}

// RuleCount is how often a rule fired.
type RuleCount struct {
	Rule     string `json:"rule"`
	Title    string `json:"title"`
	Severity string `json:"severity"`
	Count    int    `json:"count"`
}

// Options filter the report.
type Options struct {
	// MinSeverity drops findings and vulnerabilities below it ("" keeps all).
	MinSeverity string
}

// Build assembles the report for repo.
func Build(ctx context.Context, r store.Reader, repo string, opts Options) (Report, error) {
	svc := query.Service{R: r}
	rep := Report{Repo: repo, FindingCounts: map[string]int{}, VulnCounts: map[string]int{}, Findings: []Finding{}, Vulnerabilities: []Vulnerability{}, Rules: []RuleCount{}}
	minRank := model.SeverityRank(opts.MinSeverity)

	findings, err := r.List(ctx, repo, store.Filter{Kinds: []model.NodeKind{model.KindFinding}}, store.ByName, store.MaxLimit)
	if err != nil {
		return rep, err
	}
	rules := map[string]*RuleCount{}
	exposure := map[string]bool{}
	penalty := 0.0
	for _, f := range findings {
		n := f.Node
		rep.FindingCounts[n.Severity]++
		rc := rules[n.Rule]
		if rc == nil {
			rc = &RuleCount{Rule: n.Rule, Title: n.Name, Severity: n.Severity}
			rules[n.Rule] = rc
		}
		rc.Count++
		fv := Finding{ID: n.ID, Rule: n.Rule, Title: n.Name, Severity: n.Severity, CWE: n.CWE, Location: n.Location(), Detail: n.Summary, Language: findingLanguage(n)}
		owners, err := r.Traverse(ctx, n.ID, []model.EdgeKind{model.EdgeHasFinding}, store.In, 1, 1)
		if err != nil {
			return rep, err
		}
		if len(owners) > 0 {
			o := owners[0].Node
			ref := query.RefOf(o)
			fv.Owner = &ref
			switch o.Kind {
			case model.KindFile, model.KindConfig, model.KindConfigEntry, model.KindTemplate, model.KindRepository:
				// Package-level declarations, deployed config and
				// repository-wide settings are always live.
				fv.Exposed = true
			default:
				exp, ok := exposure[o.ID]
				if !ok {
					exp, err = exposed(ctx, r, o)
					if err != nil {
						return rep, err
					}
					exposure[o.ID] = exp
				}
				fv.Exposed = exp
				tests, err := svc.Tests(ctx, o.ID, 3, 1)
				if err != nil {
					return rep, err
				}
				fv.Tested = len(tests) > 0
			}
		}
		fv.Priority = severityBase[n.Severity]
		w := float64(severityWeight[n.Severity])
		if fv.Exposed {
			fv.Priority += 10
		} else {
			w /= 2
		}
		if !fv.Tested {
			fv.Priority += 5
		}
		penalty += w
		if model.SeverityRank(n.Severity) >= minRank {
			rep.Findings = append(rep.Findings, fv)
		}
	}

	vulns, err := r.List(ctx, repo, store.Filter{Kinds: []model.NodeKind{model.KindVulnerability}}, store.ByName, store.MaxLimit)
	if err != nil {
		return rep, err
	}
	vulnerableMods := map[string]bool{}
	// Vulnerabilities only required in go.mod are capped so a long tail of
	// unreachable advisories cannot dominate the grade.
	requiredPenalty := 0.0
	for _, v := range vulns {
		n := v.Node
		rep.VulnCounts[n.Severity]++
		vulnerableMods[n.Package] = true
		vv := Vulnerability{
			ID: n.Name, Aliases: n.Aliases, Summary: n.Summary, Severity: n.Severity, CWE: n.CWE,
			Module: n.Package, Version: n.Version, Fixed: n.FixedVersion, Reachability: n.Reachability, URL: n.URL,
		}
		for _, a := range n.Aliases {
			if strings.HasPrefix(a, "CVE-") {
				vv.CVE = append(vv.CVE, a)
			}
		}
		w := float64(severityWeight[n.Severity])
		vv.Priority = severityBase[n.Severity]
		switch n.Reachability {
		case "called":
			vv.Priority += 10
			sites, err := callSites(ctx, r, n.ID)
			if err != nil {
				return rep, err
			}
			vv.CallSites = sites
		case "imported":
			w /= 2
		default:
			vv.Priority -= 25
			requiredPenalty += w / 4
			w = 0
		}
		penalty += w
		if model.SeverityRank(n.Severity) >= minRank {
			rep.Vulnerabilities = append(rep.Vulnerabilities, vv)
		}
	}

	mods, err := r.List(ctx, repo, store.Filter{Kinds: []model.NodeKind{model.KindModule}}, store.ByName, store.MaxLimit)
	if err != nil {
		return rep, err
	}
	for _, m := range mods {
		if m.Node.Name == "stdlib" {
			rep.Modules.Stdlib = m.Node.Version
			continue
		}
		rep.Modules.Total++
		if m.Node.Indirect {
			rep.Modules.Indirect++
		} else {
			rep.Modules.Direct++
		}
	}
	rep.Modules.Vulnerable = len(vulnerableMods)

	sensitive, err := r.List(ctx, repo, store.Filter{Kinds: []model.NodeKind{model.KindFunction, model.KindMethod}, ExcludeTests: true}, store.ByFanIn, store.MaxLimit)
	if err != nil {
		return rep, err
	}
	for _, s := range sensitive {
		if len(s.Node.SecurityTags) > 0 && len(rep.Sensitive) < 25 {
			ref := query.RefOf(s.Node)
			ref.Summary = strings.Join(s.Node.SecurityTags, ", ")
			rep.Sensitive = append(rep.Sensitive, ref)
		}
	}

	for _, rc := range rules {
		rep.Rules = append(rep.Rules, *rc)
	}
	slices.SortFunc(rep.Rules, func(a, b RuleCount) int {
		return cmp.Or(cmp.Compare(model.SeverityRank(b.Severity), model.SeverityRank(a.Severity)), cmp.Compare(b.Count, a.Count), cmp.Compare(a.Rule, b.Rule))
	})
	slices.SortFunc(rep.Findings, func(a, b Finding) int {
		return cmp.Or(cmp.Compare(b.Priority, a.Priority), cmp.Compare(a.Location, b.Location))
	})
	slices.SortFunc(rep.Vulnerabilities, func(a, b Vulnerability) int {
		return cmp.Or(cmp.Compare(b.Priority, a.Priority), cmp.Compare(a.Module, b.Module), cmp.Compare(a.ID, b.ID))
	})
	penalty += min(requiredPenalty, 10)
	rep.Score = max(0, 100-int(penalty+0.5))
	rep.Grade = grade(rep.Score)
	return rep, nil
}

func grade(score int) string {
	switch {
	case score >= 90:
		return "A"
	case score >= 75:
		return "B"
	case score >= 60:
		return "C"
	case score >= 40:
		return "D"
	default:
		return "F"
	}
}

// IsEntryPoint reports functions callable from outside the program's own
// call graph: main/init, exported API, and handler-shaped functions.
func IsEntryPoint(n model.Node) bool {
	if n.Name == "main" || n.Name == "init" {
		return true
	}
	if n.Exported && !strings.Contains(n.Package, "/internal/") && !strings.HasSuffix(n.Package, "/internal") {
		return true
	}
	sig := n.Signature
	return strings.Contains(sig, "http.ResponseWriter") || strings.Contains(sig, "*http.Request") ||
		strings.Contains(sig, "*mcp.CallToolRequest") || strings.Contains(sig, "cobra.Command")
}

func exposed(ctx context.Context, r store.Reader, owner model.Node) (bool, error) {
	if IsEntryPoint(owner) {
		return true, nil
	}
	callers, err := r.Traverse(ctx, owner.ID, []model.EdgeKind{model.EdgeCalls, model.EdgeDispatchesTo}, store.In, exposureDepth, store.MaxLimit)
	if err != nil {
		return false, err
	}
	for _, c := range callers {
		if !c.Node.IsTest && IsEntryPoint(c.Node) {
			return true, nil
		}
	}
	return false, nil
}

func callSites(ctx context.Context, r store.Reader, vulnID string) ([]query.Ref, error) {
	syms, err := r.Traverse(ctx, vulnID, []model.EdgeKind{model.EdgeAffects}, store.Out, 1, 50)
	if err != nil {
		return nil, err
	}
	var out []query.Ref
	seen := map[string]bool{}
	for _, s := range syms {
		if s.Node.Kind != model.KindExternal {
			continue
		}
		callers, err := r.Traverse(ctx, s.Node.ID, []model.EdgeKind{model.EdgeCalls}, store.In, 1, 20)
		if err != nil {
			return nil, err
		}
		for _, c := range callers {
			if seen[c.Node.ID] {
				continue
			}
			seen[c.Node.ID] = true
			ref := query.RefOf(c.Node)
			ref.Summary = "calls " + s.Node.QualifiedName
			out = append(out, ref)
		}
	}
	return out, nil
}

// ExceedsThreshold reports whether the report has a finding or a called or
// imported vulnerability at or above severity. Required-only vulnerabilities
// do not fail the threshold.
func (r Report) ExceedsThreshold(severity string) bool {
	rank := model.SeverityRank(severity)
	if rank < 0 {
		return false
	}
	for _, f := range r.Findings {
		if model.SeverityRank(f.Severity) >= rank {
			return true
		}
	}
	for _, v := range r.Vulnerabilities {
		if v.Reachability != "required" && model.SeverityRank(v.Severity) >= rank {
			return true
		}
	}
	return false
}

// WriteMarkdown renders the report for humans and agents.
func WriteMarkdown(w io.Writer, r Report) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Security review: %s\n\n", r.Repo)
	fmt.Fprintf(&b, "**Grade %s (%d/100)** · findings: %s · vulnerabilities: %s · %d modules (%d direct), %d vulnerable",
		r.Grade, r.Score, counts(r.FindingCounts), counts(r.VulnCounts), r.Modules.Total, r.Modules.Direct, r.Modules.Vulnerable)
	if r.Modules.Stdlib != "" {
		fmt.Fprintf(&b, " · Go %s", strings.TrimPrefix(r.Modules.Stdlib, "v"))
	}
	b.WriteString("\n\n")
	if len(r.Vulnerabilities) > 0 {
		b.WriteString("## Vulnerable dependencies\n\n| Sev | ID | CVE | Module | Version → Fixed | Reach | Summary |\n|---|---|---|---|---|---|---|\n")
		for _, v := range r.Vulnerabilities {
			fixed := v.Fixed
			if fixed == "" {
				fixed = "no fix"
			}
			fmt.Fprintf(&b, "| %s | [%s](%s) | %s | `%s` | %s → %s | %s | %s |\n",
				v.Severity, v.ID, v.URL, strings.Join(v.CVE, " "), v.Module, v.Version, fixed, v.Reachability, v.Summary)
		}
		b.WriteString("\n")
	}
	if len(r.Findings) > 0 {
		b.WriteString("## Code findings (highest priority first)\n\n| Sev | Rule | Location | In | Exposed | Tested | Detail |\n|---|---|---|---|---|---|---|\n")
		for _, f := range r.Findings {
			owner := ""
			if f.Owner != nil {
				owner = "`" + f.Owner.Name + "`"
			}
			fmt.Fprintf(&b, "| %s | %s %s | %s | %s | %s | %s | %s |\n",
				f.Severity, f.Rule, strings.Join(f.CWE, " "), f.Location, owner, yesNo(f.Exposed), yesNo(f.Tested), f.Detail)
		}
		b.WriteString("\n")
	}
	if len(r.Findings) == 0 && len(r.Vulnerabilities) == 0 {
		b.WriteString("No findings at or above the selected severity.\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func counts(m map[string]int) string {
	var parts []string
	for i := len(model.Severities) - 1; i >= 0; i-- {
		s := model.Severities[i]
		if m[s] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", m[s], s))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// findingLanguage names the language of a finding for fix examples.
func findingLanguage(n model.Node) string {
	switch {
	case n.Language != "":
		return n.Language
	case strings.HasSuffix(n.File, ".go"):
		return "go"
	case strings.Contains(n.File, ".github/workflows/"):
		return "github-actions"
	case strings.HasSuffix(n.File, ".yaml"), strings.HasSuffix(n.File, ".yml"):
		return "yaml"
	case strings.HasSuffix(n.File, ".json"), strings.HasSuffix(n.File, ".json5"), strings.HasSuffix(n.File, ".renovaterc"):
		return "json"
	}
	return ""
}
