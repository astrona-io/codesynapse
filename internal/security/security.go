// Package security assembles the security review of a repository from the
// graph: code findings from the rule engine and known vulnerabilities in
// dependencies, each with severity, CWE, location, exposure (reachable from
// an entry point), test reachability and a priority that combines them.
package security

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/astrona-io/codesynapse/internal/hygiene"
	"github.com/astrona-io/codesynapse/internal/imagescan"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/query"
	"github.com/astrona-io/codesynapse/internal/redact"
	"github.com/astrona-io/codesynapse/internal/store"
)

// exposureDepth bounds the caller search for entry points.
const exposureDepth = 5

var severityWeight = map[string]int{"critical": 25, "high": 10, "medium": 3, "low": 1, "info": 0}

var severityBase = map[string]int{"critical": 90, "high": 70, "medium": 45, "low": 20, "info": 5}

// Finding is a code finding with context.
type Finding struct {
	SuppressionReason string `json:"suppression_reason,omitempty"`
	// Triage is the organisation's recorded decision when it is what
	// suppresses the finding.
	Triage *Triage `json:"triage,omitempty"`
	// IgnoreRequest is the reason of a codesynapse:ignore comment next to the
	// finding. It suppresses only when the tenant approved inline ignores.
	IgnoreRequest string `json:"ignore_request,omitempty"`
	ID            string `json:"id"`
	// Fingerprint is the finding's stable key (model.FindingFingerprint):
	// the same across line moves and re-indexing.
	Fingerprint string     `json:"fingerprint,omitempty"`
	Rule        string     `json:"rule"`
	Title       string     `json:"title"`
	Severity    string     `json:"severity"`
	CWE         []string   `json:"cwe,omitempty"`
	Location    string     `json:"location"`
	Language    string     `json:"language,omitempty"`
	Detail      string     `json:"detail"`
	Owner       *query.Ref `json:"owner,omitempty"`
	// CodeOwners are the CODEOWNERS owners of the finding's file.
	CodeOwners []string `json:"code_owners,omitempty"`
	// Exposed means the owner is an entry point or reachable from one
	// (main, an exported function, or an HTTP/MCP handler) within a few hops.
	Exposed bool `json:"exposed"`
	// Tested means a test function reaches the owner.
	Tested   bool `json:"tested"`
	Priority int  `json:"priority"`
}

// Vulnerability is a known vulnerability in a dependency.
type Vulnerability struct {
	ID       string   `json:"id"`
	CVE      []string `json:"cve,omitempty"`
	Aliases  []string `json:"aliases,omitempty"`
	Summary  string   `json:"summary"`
	Severity string   `json:"severity"`
	CWE      []string `json:"cwe,omitempty"`
	Module   string   `json:"module"`
	// Ecosystem is OSV's name for the package's ecosystem ("Go", "npm", …).
	Ecosystem    string      `json:"ecosystem,omitempty"`
	Version      string      `json:"version"`
	Fixed        string      `json:"fixed,omitempty"`
	Reachability string      `json:"reachability"`
	URL          string      `json:"url"`
	CallSites    []query.Ref `json:"call_sites,omitempty"`
	Priority     int         `json:"priority"`
	// Via and Roots say how an indirect module got in (see model.Node): the
	// chain from a dependency the project declares, and how many such
	// dependencies lead to it. Upgrading the first link of Via is usually how
	// an indirect vulnerability is fixed.
	Via   string `json:"via,omitempty"`
	Roots int    `json:"roots,omitempty"`
}

// Report is the security review.
type Report struct {
	Suppressed       []Finding      `json:"suppressed,omitempty"`
	SuppressedCounts map[string]int `json:"suppressed_counts,omitempty"`
	Repo             string         `json:"repo"`
	// Scope says what a narrowed report covers (OnlyFiles); empty for the
	// whole repository.
	Scope           string          `json:"scope,omitempty"`
	Score           int             `json:"score"`
	Grade           string          `json:"grade"`
	FindingCounts   map[string]int  `json:"finding_counts"`
	VulnCounts      map[string]int  `json:"vuln_counts"`
	Findings        []Finding       `json:"findings"`
	Vulnerabilities []Vulnerability `json:"vulnerabilities"`
	// Deprecated lists deprecated dependencies, direct ones first.
	Deprecated []DeprecatedModule `json:"deprecated,omitempty"`
	// Retracted lists Go module versions their authors retracted, direct
	// ones first; Reason is the retraction rationale.
	Retracted []DeprecatedModule `json:"retracted,omitempty"`
	// LowScorecard lists dependencies whose source project scores below
	// hygiene.LowScorecard on OpenSSF Scorecard, lowest first. It is a
	// hygiene note, never a vulnerability, and does not change the grade.
	LowScorecard []ScorecardModule `json:"low_scorecard,omitempty"`
	Modules      ModuleStats       `json:"modules"`
	Rules        []RuleCount       `json:"rules"`
	// OwnerCounts are the listed findings per CODEOWNERS owner, most
	// severe work first; empty without a CODEOWNERS file.
	OwnerCounts []OwnerCount `json:"owners,omitempty"`
	Sensitive   []query.Ref  `json:"sensitive_functions"`
	// Head and IndexedAt say which commit the report describes; a checkout
	// that moved on needs a sync before the report covers it.
	Head      string `json:"head,omitempty"`
	IndexedAt string `json:"indexed_at,omitempty"`
	// Coverage says what the rules could read; Checked and NotChecked are
	// the same in words. Nil for repositories indexed before it existed.
	Coverage   *Coverage `json:"coverage,omitempty"`
	Checked    []string  `json:"checked,omitempty"`
	NotChecked []string  `json:"not_checked,omitempty"`
	// Unanalysed counts findings beyond the first store.MaxLimit: they count
	// towards the grade at full weight but are not listed or analysed for
	// exposure.
	Unanalysed int `json:"unanalysed_findings,omitempty"`
	// ContainerImages counts the findings of the last image scan uploaded
	// from CI (internal/imagescan). Build leaves it nil: the scan lives in
	// the accounts database, so the API and MCP fill it for their caller.
	ContainerImages *imagescan.Overview `json:"container_images,omitempty"`
}

// NotApplicable is the grade when no rule had anything to read.
const NotApplicable = "N/A"

// ModuleStats counts dependencies.
type ModuleStats struct {
	Total      int `json:"total"`
	Direct     int `json:"direct"`
	Indirect   int `json:"indirect"`
	Vulnerable int `json:"vulnerable"`
	// Stdlib is the Go version checked for standard library vulnerabilities.
	Stdlib string `json:"stdlib,omitempty"`
	// Deprecated and Behind count modules deps.dev reports deprecated, or
	// behind their latest release (only with deps.dev lookups on).
	Deprecated int `json:"deprecated,omitempty"`
	Behind     int `json:"behind,omitempty"`
	// Retracted counts Go module versions the module proxy reports
	// retracted (only with deps.dev lookups on).
	Retracted int `json:"retracted,omitempty"`
	// LowScorecard counts modules whose source project scores below
	// hygiene.LowScorecard on OpenSSF Scorecard (only with deps.dev
	// lookups on).
	LowScorecard int `json:"low_scorecard,omitempty"`
}

// ScorecardModule is a dependency with the OpenSSF Scorecard of its source
// project: the overall score, the day it was produced and up to three of
// the lowest checks as "Name:score".
type ScorecardModule struct {
	Module    string   `json:"module"`
	Ecosystem string   `json:"ecosystem,omitempty"`
	Version   string   `json:"version"`
	Score     float64  `json:"score"`
	Date      string   `json:"date"`
	Checks    []string `json:"checks,omitempty"`
	Direct    bool     `json:"direct"`
}

// DeprecatedModule is a dependency whose version its publisher deprecated
// or, in Report.Retracted, retracted.
type DeprecatedModule struct {
	Module    string `json:"module"`
	Ecosystem string `json:"ecosystem,omitempty"`
	Version   string `json:"version"`
	Latest    string `json:"latest,omitempty"`
	Reason    string `json:"reason"`
	Direct    bool   `json:"direct"`
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
	// SummaryOnly skips test coverage, call sites, dependency paths and sensitive
	// symbols. Counts, exposure and grading use the same rules as a full report.
	SummaryOnly bool
	// Suppression returns a reason only for an active, authorised tenant approval.
	// Raw findings and findings beyond the report limit remain unchanged.
	Suppression func(model.Node) string
	// Triage holds the organisation's active triage decisions by finding
	// fingerprint. A finding is suppressed when Suppression or Triage says
	// so; the approved policy's reason wins when both do.
	Triage map[string]Triage
	// MinSeverity drops findings and vulnerabilities below it ("" keeps all).
	MinSeverity string
}

// Build assembles the report for repo.
func Build(ctx context.Context, r store.Reader, repo string, opts Options) (Report, error) {
	svc := query.Service{R: r}
	rep := Report{Repo: repo, SuppressedCounts: map[string]int{}, FindingCounts: map[string]int{}, VulnCounts: map[string]int{}, Findings: []Finding{}, Vulnerabilities: []Vulnerability{}, Rules: []RuleCount{}}
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
		fv := Finding{ID: n.ID, Rule: n.Rule, Title: n.Name, Severity: n.Severity, CWE: n.CWE, Location: n.Location(), Detail: n.Summary, Language: findingLanguage(n)}
		fv.IgnoreRequest = redact.Summary(redact.Free(n.Ignore), 200)
		owners, err := r.Traverse(ctx, n.ID, []model.EdgeKind{model.EdgeHasFinding}, store.In, 1, 1)
		if err != nil {
			return rep, err
		}
		var ownerNode *model.Node
		if len(owners) > 0 {
			ownerNode = &owners[0].Node
		}
		fv.Fingerprint = model.FindingFingerprint(n.Rule, model.FindingOwnerName(ownerNode, n.File), n.File)
		if len(owners) > 0 {
			o := owners[0].Node
			ref := query.RefOf(o)
			fv.Owner = &ref
			switch o.Kind {
			case model.KindFile, model.KindConfig, model.KindConfigEntry, model.KindTemplate, model.KindRepository, model.KindCommit:
				// Package-level declarations, deployed config,
				// repository-wide settings and history (in every clone)
				// are always live.
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
				if !opts.SummaryOnly {
					tests, err := svc.Tests(ctx, o.ID, 3, 1)
					if err != nil {
						return rep, err
					}
					fv.Tested = len(tests) > 0
				}
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
		if opts.Suppression != nil {
			fv.SuppressionReason = redact.Summary(redact.Free(strings.TrimSpace(opts.Suppression(n))), 500)
		}
		if t, ok := opts.Triage[fv.Fingerprint]; ok && fv.SuppressionReason == "" {
			fv.Triage = &t
			fv.SuppressionReason = t.Label()
		}
		if fv.SuppressionReason != "" {
			rep.SuppressedCounts[n.Severity]++
			rep.Suppressed = append(rep.Suppressed, fv)
			continue
		}
		rep.FindingCounts[n.Severity]++
		rc := rules[n.Rule]
		if rc == nil {
			rc = &RuleCount{Rule: n.Rule, Title: n.Name, Severity: n.Severity}
			rules[n.Rule] = rc
		}
		rc.Count++
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
			Module: n.Package, Ecosystem: n.Ecosystem, Version: n.Version, Fixed: n.FixedVersion, Reachability: n.Reachability, URL: n.URL,
		}
		key := n.Package
		if n.Ecosystem != "" && n.Ecosystem != "Go" {
			key = n.Ecosystem + ":" + n.Package
		}
		if !opts.SummaryOnly {
			// A package can be installed in several versions; the advisory hit
			// one of them, whose chain of dependents is the one to show.
			mod, err := r.Node(ctx, model.NodeID(repo, model.KindModule, key+"@"+n.Version))
			if err != nil {
				mod, err = r.Node(ctx, model.NodeID(repo, model.KindModule, key))
			}
			if err == nil {
				vv.Via, vv.Roots = mod.Via, mod.Roots
			}
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
			if !opts.SummaryOnly {
				sites, err := callSites(ctx, r, n.ID)
				if err != nil {
					return rep, err
				}
				vv.CallSites = sites
			}
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
		if m.Node.Behind > 0 {
			rep.Modules.Behind++
		}
		if m.Node.Deprecated != "" {
			rep.Modules.Deprecated++
			rep.Deprecated = append(rep.Deprecated, DeprecatedModule{Module: m.Node.Name, Ecosystem: m.Node.Ecosystem, Version: m.Node.Version,
				Latest: m.Node.Latest, Reason: m.Node.Deprecated, Direct: !m.Node.Indirect})
		}
		if m.Node.Retracted != "" {
			rep.Modules.Retracted++
			rep.Retracted = append(rep.Retracted, DeprecatedModule{Module: m.Node.Name, Ecosystem: m.Node.Ecosystem, Version: m.Node.Version,
				Latest: m.Node.Latest, Reason: m.Node.Retracted, Direct: !m.Node.Indirect})
		}
		if m.Node.ScorecardDate != "" && m.Node.Scorecard < hygiene.LowScorecard {
			rep.Modules.LowScorecard++
			rep.LowScorecard = append(rep.LowScorecard, ScorecardModule{Module: m.Node.Name, Ecosystem: m.Node.Ecosystem, Version: m.Node.Version,
				Score: m.Node.Scorecard, Date: m.Node.ScorecardDate, Checks: m.Node.ScorecardChecks, Direct: !m.Node.Indirect})
		}
	}
	directFirst := func(a, b DeprecatedModule) int {
		if a.Direct != b.Direct {
			return map[bool]int{true: -1, false: 1}[a.Direct]
		}
		return cmp.Compare(a.Module, b.Module)
	}
	slices.SortStableFunc(rep.Deprecated, directFirst)
	slices.SortStableFunc(rep.Retracted, directFirst)
	slices.SortStableFunc(rep.LowScorecard, func(a, b ScorecardModule) int {
		return cmp.Or(cmp.Compare(a.Score, b.Score), directFirst(DeprecatedModule{Module: a.Module, Direct: a.Direct}, DeprecatedModule{Module: b.Module, Direct: b.Direct}))
	})
	rep.Modules.Vulnerable = len(vulnerableMods)
	// The list stops at MaxLimit; the node count does not. Modules beyond the
	// list are counted as indirect, which large lockfiles mostly are.
	if len(mods) == store.MaxLimit {
		st, err := r.Stats(ctx, repo)
		if err != nil {
			return rep, err
		}
		all := st.Nodes[model.KindModule]
		if rep.Modules.Stdlib != "" {
			all--
		}
		if all > rep.Modules.Total {
			rep.Modules.Indirect += all - rep.Modules.Total
			rep.Modules.Total = all
		}
	}

	if !opts.SummaryOnly {
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
	// List stops at MaxLimit; findings beyond it still count, at full
	// weight since their exposure was not analysed.
	all, err := r.FindingCounts(ctx, repo)
	if err != nil {
		return rep, err
	}
	for sev, total := range all {
		if extra := total - rep.FindingCounts[sev] - rep.SuppressedCounts[sev]; extra > 0 {
			penalty += float64(extra * severityWeight[sev])
			rep.Unanalysed += extra
			rep.FindingCounts[sev] = total - rep.SuppressedCounts[sev]
		}
	}
	penalty += min(requiredPenalty, 10)
	rep.Score = max(0, 100-int(penalty+0.5))
	rep.Grade = grade(rep.Score)

	if n, err := r.Node(ctx, model.NodeID(repo, model.KindRepository, repo)); err == nil {
		rep.Head, rep.IndexedAt = n.Head, n.Time
		rep.applyOwners(model.CodeOwnersOf(n))
		var c Coverage
		if n.Coverage != "" && json.Unmarshal([]byte(n.Coverage), &c) == nil {
			rep.Coverage = &c
			rep.Checked, rep.NotChecked = c.Lines()
			if !c.Scannable() {
				// An A for a repository nothing could be checked in would
				// read as a clean bill of health.
				rep.Score, rep.Grade = 0, NotApplicable
			}
		}
	}
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
	if r.Unanalysed > 0 {
		for severity, count := range r.FindingCounts {
			if count > 0 && model.SeverityRank(severity) >= rank {
				return true
			}
		}
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
	if r.Scope != "" {
		fmt.Fprintf(&b, "**Scope: %s** · findings: %s\n\n", r.Scope, counts(r.FindingCounts))
	} else {
		writeHeader(&b, r)
	}
	if r.Unanalysed > 0 {
		fmt.Fprintf(&b, "%d more findings are counted in the grade but not listed (the report lists the first %d).\n\n", r.Unanalysed, store.MaxLimit)
	}
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
	moduleTable(&b, "Deprecated dependencies", "Reason", r.Deprecated)
	moduleTable(&b, "Retracted Go module versions", "Rationale", r.Retracted)
	if len(r.LowScorecard) > 0 {
		fmt.Fprintf(&b, "## Low OpenSSF Scorecard\n\nA hygiene note, not a vulnerability: the source projects of these dependencies score below %.1f of 10 on [OpenSSF Scorecard](https://scorecard.dev) (from deps.dev).\n\n", hygiene.LowScorecard)
		b.WriteString("| Module | Version | Score | Direct | Lowest checks | Scored on |\n|---|---|---|---|---|---|\n")
		for _, s := range r.LowScorecard {
			checks := strings.Join(s.Checks, ", ")
			fmt.Fprintf(&b, "| `%s` | %s | %.1f | %s | %s | %s |\n", s.Module, s.Version, s.Score, yesNo(s.Direct), cmp.Or(checks, "–"), s.Date)
		}
		b.WriteString("\n")
	}
	if len(r.OwnerCounts) > 0 {
		b.WriteString("## Findings by owner (CODEOWNERS)\n\n| Owner | Findings | By severity |\n|---|---|---|\n")
		for _, o := range r.OwnerCounts {
			fmt.Fprintf(&b, "| %s | %d | %s |\n", cmp.Or(o.Owner, "no owner"), o.Findings, counts(o.Counts))
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
			if len(f.CodeOwners) > 0 {
				owner = strings.TrimSpace(owner + " " + strings.Join(f.CodeOwners, " "))
			}
			fmt.Fprintf(&b, "| %s | %s %s | %s | %s | %s | %s | %s |\n",
				f.Severity, f.Rule, strings.Join(f.CWE, " "), f.Location, owner, yesNo(f.Exposed), yesNo(f.Tested), f.Detail)
		}
		b.WriteString("\n")
	}
	if len(r.Suppressed) > 0 {
		fmt.Fprintf(&b, "## Approved suppressions and triage\n\n%s. These findings remain stored and are excluded from this tenant's grade.\n\n| Sev | Rule | Location | Reason |\n|---|---|---|---|\n", counts(r.SuppressedCounts))
		escape := strings.NewReplacer("|", "\\|", "\n", " ", "\r", " ")
		for _, f := range r.Suppressed {
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", f.Severity, f.Rule, escape.Replace(f.Location), escape.Replace(f.SuppressionReason))
		}
		b.WriteString("\n")
	}
	if len(r.Findings) == 0 && len(r.Vulnerabilities) == 0 {
		b.WriteString("No active findings at or above the selected severity.\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// writeHeader is the grade, dependency counts, commit and coverage of a
// report on the whole repository.
func writeHeader(b *strings.Builder, r Report) {
	if r.Grade == NotApplicable {
		b.WriteString("**No grade:** nothing in this repository is covered by the security rules (see below).")
	} else {
		fmt.Fprintf(b, "**Grade %s (%d/100)**", r.Grade, r.Score)
	}
	fmt.Fprintf(b, " · findings: %s · vulnerabilities: %s · %d modules (%d direct), %d vulnerable",
		counts(r.FindingCounts), counts(r.VulnCounts), r.Modules.Total, r.Modules.Direct, r.Modules.Vulnerable)
	if r.Modules.Stdlib != "" {
		fmt.Fprintf(b, " · Go %s", strings.TrimPrefix(r.Modules.Stdlib, "v"))
	}
	b.WriteString("\n\n")
	if r.Head != "" {
		fmt.Fprintf(b, "Indexed at commit `%s` (%s). If your checkout is on another commit, sync the repository first.\n\n", short(r.Head), r.IndexedAt)
	}
	if len(r.Checked) > 0 || len(r.NotChecked) > 0 {
		b.WriteString("**What was checked:**\n")
		for _, c := range r.Checked {
			fmt.Fprintf(b, "- %s\n", c)
		}
		if len(r.NotChecked) > 0 {
			b.WriteString("\n**Not checked (review by hand):**\n")
			for _, c := range r.NotChecked {
				fmt.Fprintf(b, "- %s\n", c)
			}
		}
		b.WriteString("\n")
	}
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

func short(h string) string {
	if len(h) > 10 {
		return h[:10]
	}
	return h
}

// moduleTable writes a section listing deprecated or retracted modules;
// nothing when mods is empty.
func moduleTable(b *strings.Builder, title, reason string, mods []DeprecatedModule) {
	if len(mods) == 0 {
		return
	}
	fmt.Fprintf(b, "## %s\n\n| Module | Version | Latest | Direct | %s |\n|---|---|---|---|---|\n", title, reason)
	escape := strings.NewReplacer("|", "\\|", "\n", " ")
	for _, d := range mods {
		fmt.Fprintf(b, "| `%s` | %s | %s | %s | %s |\n", d.Module, d.Version, cmp.Or(d.Latest, "?"), yesNo(d.Direct), escape.Replace(d.Reason))
	}
	b.WriteString("\n")
}
