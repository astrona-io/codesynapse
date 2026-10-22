// Package cleanupplan turns the findings CodeSynapse already has (unused
// code and dependencies, vulnerable dependencies, licence conflicts,
// duplicate code and untested hotspots) into one ranked list of changes.
//
// The ranking is arithmetic anyone can read: a step has a value (how much it
// is worth, 0 to 100), an effort (1 to 3) and a safety (how likely it is to be
// right without a person looking closely), and
//
//	priority = value x safety factor / effort divisor
//
// with safety factors 1.0, 0.8 and 0.6 for high, medium and low, and effort
// divisors 1.0, 1.5 and 2.0. Value, effort and safety are shown on every
// step, so a different order is a judgement the reader can make. Nothing here
// edits code: the plan says what to look at, why and how to check it.
package cleanupplan

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/parisnakitakejser/codesynapse/internal/cleanup"
	"github.com/parisnakitakejser/codesynapse/internal/duplicates"
	"github.com/parisnakitakejser/codesynapse/internal/license"
	"github.com/parisnakitakejser/codesynapse/internal/query"
	"github.com/parisnakitakejser/codesynapse/internal/security"
)

// Step kinds.
const (
	RemoveRiskyDeadCode = "remove-risky-dead-code"
	RemoveDeadCode      = "remove-dead-code"
	RemoveDeadFile      = "remove-dead-file"
	RemoveDependency    = "remove-dependency"
	UpgradeDependency   = "upgrade-dependency"
	ResolveLicence      = "resolve-licence"
	MergeDuplicates     = "merge-duplicates"
	ReviewEditedCopies  = "review-edited-copies"
	TestHotspot         = "test-hotspot"
	maxTargets          = 8
	maxStepsPerKind     = 10
	maxHotspotSteps     = 5
	maxEditedCopySteps  = 5
	vulnMinSeverityRank = 2 // medium and above are planned unless the code calls them
)

// Hotspot is a hotspot and whether any test reaches it.
type Hotspot struct {
	query.Hotspot
	Tested bool `json:"tested"`
}

// Inputs are the reports the plan is built from; a nil one is left out.
type Inputs struct {
	Repo       string
	Cleanup    *cleanup.Report
	Duplicates *duplicates.Report
	Vulns      []security.Vulnerability
	Licences   *license.Report
	Hotspots   []Hotspot
}

// Target is a place a step is about.
type Target struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name"`
	Location string `json:"location,omitempty"`
}

// Step is one change worth making.
type Step struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Title    string   `json:"title"`
	Why      string   `json:"why"`
	Value    int      `json:"value"`
	Effort   int      `json:"effort"`
	Safety   string   `json:"safety"`
	Priority float64  `json:"priority"`
	Lines    int      `json:"lines,omitempty"`
	Advisory int      `json:"advisories,omitempty"`
	Targets  []Target `json:"targets"`
	Verify   string   `json:"verify"`
	// Link is a path in the web app with more detail.
	Link string `json:"link,omitempty"`
	// Group is the duplicate group a merge step is about, for refactor_duplicates.
	Group *GroupRef `json:"group,omitempty"`
}

// GroupRef names a duplicate group as duplicate_code lists it.
type GroupRef struct {
	Hash string `json:"hash"`
	Kind string `json:"kind"`
}

// Summary totals the plan. Lines can overlap between steps (a duplicate that
// is also unused), so they are an upper bound.
type Summary struct {
	Steps      int `json:"steps"`
	LinesUpTo  int `json:"lines_up_to"`
	Advisories int `json:"advisories"`
}

// Plan is the ranked list.
type Plan struct {
	Steps   []Step   `json:"steps"`
	Summary Summary  `json:"summary"`
	Notes   []string `json:"notes"`
}

var (
	safetyFactor = map[string]float64{"high": 1.0, "medium": 0.8, "low": 0.6}
	effortDiv    = map[int]float64{1: 1.0, 2: 1.5, 3: 2.0}
)

func prioritise(s *Step) {
	s.Priority = float64(int(float64(s.Value)*safetyFactor[s.Safety]/effortDiv[s.Effort]*10+0.5)) / 10
}

func stepID(kind, key string) string {
	sum := sha256.Sum256([]byte(kind + "|" + key))
	return kind + "-" + hex.EncodeToString(sum[:4])
}

// capAt limits a score to hi; scores are never negative.
func capAt(v, hi int) int { return min(hi, v) }

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// Build ranks the findings in in.
func Build(in Inputs) Plan {
	p := Plan{Steps: []Step{}, Notes: []string{}}
	add := func(s Step) {
		prioritise(&s)
		p.Steps = append(p.Steps, s)
	}
	repoLink := "/repos/" + url.PathEscape(in.Repo)

	covered := map[string]bool{} // modules removing them already fixes
	if c := in.Cleanup; c != nil {
		deadCode(c, repoLink, add, &p)
		deadFiles(c, repoLink, add)
		unusedModules(c, repoLink, add, covered)
	}
	vulnSteps(in.Vulns, covered, repoLink, add, &p)
	licenceSteps(in.Licences, repoLink, add, &p)
	if d := in.Duplicates; d != nil {
		duplicateSteps(d, repoLink, add)
	}
	hotspotSteps(in.Hotspots, repoLink, add)

	slices.SortFunc(p.Steps, func(a, b Step) int {
		return cmp.Or(cmp.Compare(b.Priority, a.Priority), cmp.Compare(a.ID, b.ID))
	})
	for _, s := range p.Steps {
		p.Summary.LinesUpTo += s.Lines
		p.Summary.Advisories += s.Advisory
	}
	p.Summary.Steps = len(p.Steps)
	return p
}

// --- dead code ----------------------------------------------------------------

func dirOf(location string) string {
	file, _, _ := strings.Cut(location, ":")
	if d := path.Dir(file); d != "." {
		return d
	}
	return "(repository root)"
}

func deadCode(c *cleanup.Report, link string, add func(Step), p *Plan) {
	risky := map[string][]cleanup.Item{}
	plain := map[string][]cleanup.Item{}
	manual := 0
	for _, it := range c.Items {
		switch {
		case it.Category != "unreachable":
			// Code only tests use is removed together with its tests; not a safe quick win.
			manual++
		case len(it.Security) > 0 && it.Confidence != "low":
			risky[dirOf(it.Location)] = append(risky[dirOf(it.Location)], it)
		case it.Confidence == "high":
			plain[dirOf(it.Location)] = append(plain[dirOf(it.Location)], it)
		default:
			manual++
		}
	}
	emit := func(groups map[string][]cleanup.Item, kind string) {
		dirs := make([]string, 0, len(groups))
		for d := range groups {
			dirs = append(dirs, d)
		}
		slices.SortFunc(dirs, func(a, b string) int {
			return cmp.Or(cmp.Compare(lines(groups[b]), lines(groups[a])), cmp.Compare(a, b))
		})
		for _, d := range dirs[:min(len(dirs), maxStepsPerKind)] {
			items := groups[d]
			slices.SortFunc(items, func(a, b cleanup.Item) int { return cmp.Or(cmp.Compare(b.LOC, a.LOC), cmp.Compare(a.Name, b.Name)) })
			loc := lines(items)
			s := Step{Kind: kind, ID: stepID(kind, d), Lines: loc, Effort: 1, Link: link + "/cleanup",
				Verify: "Build and run the tests (the compiler finds anything that still used it), then Sync and check the unused-code count went down."}
			if kind == RemoveRiskyDeadCode {
				tags := tagsOf(items)
				s.Title = fmt.Sprintf("Delete %s in %s that nothing calls and that touch %s", plural(len(items), "function", "functions"), d, strings.Join(tags, ", "))
				s.Why = "Unreachable code is still compiled, shipped and patched. Here it also reaches sensitive APIs, so it is pure risk."
				s.Value, s.Safety = capAt(75+loc/50, 90), "high"
				if slices.ContainsFunc(items, func(it cleanup.Item) bool { return it.Confidence != "high" }) {
					s.Safety = "medium"
				}
			} else {
				s.Title = fmt.Sprintf("Delete %s (%d lines) in %s that nothing calls", plural(len(items), "unreachable function", "unreachable functions"), loc, d)
				s.Why = "Nothing reaches these unexported functions from main, exported API, handlers or tests. They cost reading, review and patching and add nothing."
				s.Value, s.Safety = capAt(10+loc/10, 60), "high"
			}
			for _, it := range items[:min(len(items), maxTargets)] {
				s.Targets = append(s.Targets, Target{ID: it.ID, Name: it.Name, Location: it.Location})
			}
			add(s)
		}
	}
	emit(risky, RemoveRiskyDeadCode)
	emit(plain, RemoveDeadCode)
	if manual > 0 {
		p.Notes = append(p.Notes, fmt.Sprintf("%s (exported, test-only or low confidence) are not in the plan: check for users outside the repository, reflection and plugins before deleting them. The Unused code page lists them.", plural(manual, "unused function", "unused functions")))
	}
}

func lines(items []cleanup.Item) int {
	n := 0
	for _, it := range items {
		n += it.LOC
	}
	return n
}

func tagsOf(items []cleanup.Item) []string {
	set := map[string]bool{}
	for _, it := range items {
		for _, t := range it.Security {
			set[t] = true
		}
	}
	tags := make([]string, 0, len(set))
	for t := range set {
		tags = append(tags, t)
	}
	slices.Sort(tags)
	return tags
}

func deadFiles(c *cleanup.Report, link string, add func(Step)) {
	files := slices.Clone(c.Files)
	files = slices.DeleteFunc(files, func(f cleanup.FileItem) bool { return f.TestOnly })
	slices.SortFunc(files, func(a, b cleanup.FileItem) int { return cmp.Or(cmp.Compare(b.LOC, a.LOC), cmp.Compare(a.File, b.File)) })
	for _, f := range files[:min(len(files), maxStepsPerKind)] {
		add(Step{Kind: RemoveDeadFile, ID: stepID(RemoveDeadFile, f.File), Lines: f.LOC, Effort: 1, Safety: "medium", Link: link + "/cleanup",
			Title:   fmt.Sprintf("Delete %s: nothing in it is used", f.File),
			Why:     fmt.Sprintf("All %s in this file are unreachable. Types and constants are not tracked, so check that nothing outside Go (templates, generated code, reflection) names them.", plural(f.Symbols, "function", "functions")),
			Value:   capAt(15+f.LOC/20, 60),
			Targets: []Target{{Name: f.File, Location: f.File}},
			Verify:  "Build and run the tests, then Sync."})
	}
}

// --- dependencies -------------------------------------------------------------

func unusedModules(c *cleanup.Report, link string, add func(Step), covered map[string]bool) {
	mods := slices.Clone(c.Modules)
	slices.SortFunc(mods, func(a, b cleanup.ModuleItem) int {
		return cmp.Or(cmp.Compare(len(b.Vulns), len(a.Vulns)), cmp.Compare(a.Path, b.Path))
	})
	for _, m := range mods[:min(len(mods), maxStepsPerKind)] {
		covered[m.Path] = true
		why := "Nothing in the repository imports it, so it only adds supply-chain surface and update work."
		if len(m.Vulns) > 0 {
			why = fmt.Sprintf("Nothing imports it, and it has %s (%s): removing it removes them.", plural(len(m.Vulns), "known vulnerability", "known vulnerabilities"), strings.Join(m.Vulns[:min(len(m.Vulns), 3)], ", "))
		}
		add(Step{Kind: RemoveDependency, ID: stepID(RemoveDependency, m.Path), Effort: 1, Safety: "high", Advisory: len(m.Vulns), Link: link + "/dependency-review",
			Title: "Remove the unused dependency " + m.Path, Why: why, Value: capAt(35+len(m.Vulns)*15, 90),
			Targets: []Target{{Name: m.Path + "@" + m.Version}},
			Verify:  "Run go mod tidy (or the package manager's remove), build, run the tests, Sync, and check the advisories are gone."})
	}
	for _, m := range mods[min(len(mods), maxStepsPerKind):] {
		covered[m.Path] = true
	}
}

var sevRank = map[string]int{"low": 1, "medium": 2, "high": 3, "critical": 4}
var sevValue = map[string]int{"low": 25, "medium": 50, "high": 75, "critical": 90}

func vulnSteps(vulns []security.Vulnerability, covered map[string]bool, link string, add func(Step), p *Plan) {
	byModule := map[string][]security.Vulnerability{}
	skipped := 0
	for _, v := range vulns {
		if covered[v.Module] {
			continue
		}
		if sevRank[v.Severity] < vulnMinSeverityRank && v.Reachability != "called" {
			skipped++
			continue
		}
		byModule[v.Module] = append(byModule[v.Module], v)
	}
	mods := make([]string, 0, len(byModule))
	for m := range byModule {
		mods = append(mods, m)
	}
	best := func(m string) int {
		b := 0
		for _, v := range byModule[m] {
			b = max(b, vulnScore(v))
		}
		return b
	}
	slices.SortFunc(mods, func(a, b string) int { return cmp.Or(cmp.Compare(best(b), best(a)), cmp.Compare(a, b)) })
	for _, m := range mods[:min(len(mods), maxStepsPerKind*2)] {
		vs := byModule[m]
		first := vs[0]
		fixed := ""
		called := false
		for _, v := range vs {
			if v.Fixed != "" && fixed == "" {
				fixed = v.Fixed
			}
			called = called || v.Reachability == "called"
		}
		s := Step{Kind: UpgradeDependency, ID: stepID(UpgradeDependency, m), Advisory: len(vs), Safety: "medium", Effort: 2, Value: best(m), Link: link + "/dependency-review",
			Verify: "Upgrade with the package manager, build, run the tests, Sync, and check the advisories are gone."}
		ids := make([]string, 0, len(vs))
		for _, v := range vs {
			ids = append(ids, v.ID)
		}
		s.Targets = []Target{{Name: m + "@" + first.Version}}
		target := m
		if first.Via != "" {
			// An indirect package is fixed by bumping what pulls it in.
			parent, _, _ := strings.Cut(first.Via, " > ")
			target = parent
			s.Effort = 3
			s.Targets = append(s.Targets, Target{Name: parent, Location: "via " + first.Via})
		}
		to := "a fixed version"
		if fixed != "" {
			to = fixed
		}
		s.Title = fmt.Sprintf("Upgrade %s to fix %s", target, plural(len(vs), "advisory", "advisories"))
		s.Why = fmt.Sprintf("%s (%s) affects %s", strings.Join(ids[:min(len(ids), 3)], ", "), first.Severity, m)
		if first.Via != "" {
			s.Why += fmt.Sprintf(", which is pulled in via %s; the fix is in %s", first.Via, to)
		} else {
			s.Why += fmt.Sprintf("; fixed in %s", to)
		}
		if called {
			s.Why += ". Your code calls the vulnerable code, so this is exploitable until fixed."
		} else {
			s.Why += "."
		}
		add(s)
	}
	if skipped > 0 {
		p.Notes = append(p.Notes, fmt.Sprintf("%s of low severity that your code does not call are not in the plan; the Dependencies page lists them.", plural(skipped, "advisory", "advisories")))
	}
}

func vulnScore(v security.Vulnerability) int {
	s := sevValue[v.Severity]
	switch v.Reachability {
	case "called":
		s += 10
	case "imported":
		s += 5
	}
	return capAt(s, 100)
}

func licenceSteps(r *license.Report, link string, add func(Step), p *Plan) {
	if r == nil {
		return
	}
	n := 0
	for _, e := range r.Issues {
		if e.Verdict.Level != "conflict" || e.Dev {
			continue
		}
		if n++; n > maxStepsPerKind {
			break
		}
		add(Step{Kind: ResolveLicence, ID: stepID(ResolveLicence, e.Ecosystem+"|"+e.Name), Effort: 3, Safety: "low", Value: 85, Link: link + "/licenses",
			Title:   fmt.Sprintf("Resolve the licence conflict of %s (%s)", e.Name, cmp.Or(e.License, "unknown licence")),
			Why:     e.Verdict.Reason + " This is a flag for whoever owns licensing, not a legal finding.",
			Targets: []Target{{Name: e.Name + "@" + e.Version}},
			Verify:  "Decide with the licence owner: replace it, comply, or document the exception. Then Sync."})
	}
	if r.Review > 0 {
		p.Notes = append(p.Notes, fmt.Sprintf("%s have licences to read before relying on them; the Licences page lists them.", plural(r.Review, "dependency", "dependencies")))
	}
}

// --- duplicates and hotspots ---------------------------------------------------

func duplicateSteps(d *duplicates.Report, link string, add func(Step)) {
	exact, near := 0, 0
	for _, g := range d.Groups {
		if len(g.Members) < 2 {
			continue
		}
		targets := make([]Target, 0, maxTargets)
		for _, m := range g.Members[:min(len(g.Members), maxTargets)] {
			targets = append(targets, Target{ID: m.ID, Name: m.Name, Location: fmt.Sprintf("%s:%d-%d", m.File, m.StartLine, m.EndLine)})
		}
		name := shortName(g.Members[0].Name)
		if g.Kind == "near" {
			if near++; near > maxEditedCopySteps {
				continue
			}
			add(Step{Kind: ReviewEditedCopies, ID: stepID(ReviewEditedCopies, g.Hash+"|near"), Lines: g.Redundant, Effort: 3, Safety: "low", Link: link + "/duplicates?group=" + g.Hash + "&kind=near", Group: &GroupRef{Hash: g.Hash, Kind: "near"},
				Value:   capAt(5+g.Redundant/10, 40),
				Title:   fmt.Sprintf("Check whether %d edited copies of %s should be one function (~%.0f%% alike)", g.Copies, name, g.Similarity*100),
				Why:     "They started as copies and drifted. Sometimes the difference is the reason they exist; sometimes it is a fix that only reached one of them.",
				Targets: targets, Verify: "Read what differs. If it is incidental, merge them behind one function with a parameter; if not, leave a comment saying why."})
			continue
		}
		if exact++; exact > maxStepsPerKind {
			continue
		}
		effort := map[string]int{"file": 1, "package": 2}[g.Scope]
		effort = cmp.Or(effort, 3)
		where := map[string]string{"file": "extract a helper in the same file", "package": "extract one function in the package", "repository": "move one maintained copy into a package they all import"}[g.Scope]
		add(Step{Kind: MergeDuplicates, ID: stepID(MergeDuplicates, g.Hash), Lines: g.Redundant, Effort: effort, Safety: "medium", Link: link + "/duplicates?group=" + g.Hash + "&kind=exact", Group: &GroupRef{Hash: g.Hash, Kind: "exact"},
			Value:   capAt(10+g.Redundant/5, 70),
			Title:   fmt.Sprintf("Merge %d exact copies of %s (%d redundant lines)", g.Copies, name, g.Redundant),
			Why:     "Identical logic that has to be read, tested and fixed in every copy, and that drifts when only one is fixed. To fix it, " + where + ".",
			Targets: targets, Verify: "Run the tests of every caller you changed, Sync, and check the group is gone from the Duplicates page."})
	}
}

var pathSegment = regexp.MustCompile(`[A-Za-z0-9_.~-]+/`)

// shortName drops the import path from a qualified name:
// "(*github.com/x/y/cmd/tool.Opts).Run" becomes "(*tool.Opts).Run".
func shortName(qualified string) string { return pathSegment.ReplaceAllString(qualified, "") }

func hotspotSteps(hs []Hotspot, link string, add func(Step)) {
	n := 0
	for _, h := range hs {
		if h.Tested {
			continue
		}
		if n++; n > maxHotspotSteps {
			break
		}
		add(Step{Kind: TestHotspot, ID: stepID(TestHotspot, h.ID), Effort: 2, Safety: "medium", Link: link + "/metrics",
			Value:   capAt(30+h.Score/20, 70),
			Title:   fmt.Sprintf("Cover %s with tests before changing it again", h.Name),
			Why:     fmt.Sprintf("Complexity %d in a file with %d recent changes, called from %s, and no test reaches it. Code like this is where defects concentrate.", h.Complexity, h.FileChurn, plural(h.FanIn, "place", "places")),
			Targets: []Target{{ID: h.ID, Name: h.Name, Location: h.Location}},
			Verify:  "Add tests for the behaviour callers rely on, Sync, and check the function is reached by a test."})
	}
}
