// Package history keeps what a repository looked like after each ingest, so
// the answer to "are we getting better or worse?" is a line and not a guess.
//
// A Snapshot is a few dozen numbers taken from the reports ingest already
// stores; no code, names or paths are kept. Trend compares snapshots and
// names the changes that matter: a worse security grade, more vulnerabilities
// your code calls, more duplicate or dead code, fewer tests reaching the code.
package history

import (
	"cmp"
	"fmt"
	"math"
	"slices"
)

// MaxSnapshots is how many snapshots a repository keeps; older ones are dropped.
const MaxSnapshots = 500

// Snapshot is the state of one repository after one ingest.
type Snapshot struct {
	// At is the ingest time (RFC 3339) and identifies the snapshot.
	At   string `json:"at"`
	Head string `json:"head,omitempty"`
	// Branch is the branch that was indexed ("" when unknown: a detached
	// checkout, or a snapshot taken before branches were recorded). Snapshots
	// are compared only with earlier ones of the same branch.
	Branch string `json:"branch,omitempty"`

	Grade string `json:"grade,omitempty"`
	Score int    `json:"score"`
	// Findings and Vulns count security findings and vulnerable-dependency
	// advisories by severity.
	Findings    map[string]int `json:"findings,omitempty"`
	Vulns       map[string]int `json:"vulns,omitempty"`
	VulnsCalled int            `json:"vulns_called"`

	Modules          int `json:"modules"`
	Direct           int `json:"direct"`
	LicenceConflicts int `json:"licence_conflicts"`
	LicenceReview    int `json:"licence_review"`

	Functions     int `json:"functions"`
	FunctionLOC   int `json:"function_loc"`
	DeadFunctions int `json:"dead_functions"`
	DeadLOC       int `json:"dead_loc"`
	UnusedModules int `json:"unused_modules"`

	// DupLOC and NearLOC are redundant lines in exact and edited copies;
	// DupPct is their share of the function lines.
	DupLOC  int     `json:"dup_loc"`
	NearLOC int     `json:"near_loc"`
	DupPct  float64 `json:"dup_pct"`

	TestReachPct   float64 `json:"test_reach_pct"`
	DocCoveragePct float64 `json:"doc_coverage_pct"`
}

// gradeRank orders grades from best to worst; N/A and unknown rank worst so a
// repository that stops being checkable shows up as a change.
var gradeRank = map[string]int{"A": 0, "B": 1, "C": 2, "D": 3, "F": 4}

func rankOf(g string) int {
	if r, ok := gradeRank[g]; ok {
		return r
	}
	return 5
}

func sev(m map[string]int, s ...string) int {
	n := 0
	for _, k := range s {
		n += m[k]
	}
	return n
}

// metric is one number a trend follows.
type metric struct {
	key, label string
	// lowerIsBetter says which way an improvement goes.
	lowerIsBetter bool
	unit          string
	// threshold is the smallest change from one snapshot to the next that is
	// reported as a regression or improvement; smaller changes are noise.
	threshold float64
	get       func(Snapshot) float64
}

var metrics = []metric{
	{"score", "Security score", false, "points", 1, func(s Snapshot) float64 { return float64(s.Score) }},
	{"vulns_called", "Vulnerabilities your code calls", true, "", 1, func(s Snapshot) float64 { return float64(s.VulnsCalled) }},
	{"findings_high", "High and critical findings", true, "", 1, func(s Snapshot) float64 { return float64(sev(s.Findings, "high", "critical")) }},
	{"vulns_high", "High and critical advisories", true, "", 1, func(s Snapshot) float64 { return float64(sev(s.Vulns, "high", "critical")) }},
	{"licence_conflicts", "Licence conflicts", true, "", 1, func(s Snapshot) float64 { return float64(s.LicenceConflicts) }},
	{"dup_pct", "Duplicate code", true, "%", 0.5, func(s Snapshot) float64 { return s.DupPct }},
	{"dead_functions", "Unused functions", true, "", 5, func(s Snapshot) float64 { return float64(s.DeadFunctions) }},
	{"unused_modules", "Unused dependencies", true, "", 1, func(s Snapshot) float64 { return float64(s.UnusedModules) }},
	{"test_reach_pct", "Code reached by tests", false, "%", 2, func(s Snapshot) float64 { return s.TestReachPct }},
	{"doc_coverage_pct", "Exported symbols documented", false, "%", 3, func(s Snapshot) float64 { return s.DocCoveragePct }},
	{"modules", "Dependencies", true, "", math.MaxFloat64, func(s Snapshot) float64 { return float64(s.Modules) }},
	{"function_loc", "Function lines", true, "", math.MaxFloat64, func(s Snapshot) float64 { return float64(s.FunctionLOC) }},
}

// Series is one metric over the snapshots, in order.
type Series struct {
	Key           string    `json:"key"`
	Label         string    `json:"label"`
	Unit          string    `json:"unit,omitempty"`
	LowerIsBetter bool      `json:"lower_is_better"`
	Values        []float64 `json:"values"`
}

// Change is a metric that moved enough between two snapshots to matter.
type Change struct {
	Key   string  `json:"key"`
	Label string  `json:"label"`
	From  float64 `json:"from"`
	To    float64 `json:"to"`
	// Detail is a sentence that says what changed.
	Detail string `json:"detail"`
}

// Trend is the comparison of a repository's snapshots.
type Trend struct {
	Snapshots []Snapshot `json:"snapshots"`
	Series    []Series   `json:"series"`
	// Regressions and Improvements compare the latest snapshot with the one
	// before it.
	Regressions  []Change `json:"regressions"`
	Improvements []Change `json:"improvements"`
	// Since is the time of the snapshot compared with.
	Since string `json:"since,omitempty"`
	// Branches is the security trend of each branch, the branch of the
	// latest snapshot first.
	Branches []BranchTrend `json:"branches"`
}

// SecurityPoint is the security state after one ingest of a branch.
type SecurityPoint struct {
	At       string `json:"at"`
	Head     string `json:"head,omitempty"`
	Grade    string `json:"grade,omitempty"`
	Score    int    `json:"score"`
	Critical int    `json:"critical"`
	High     int    `json:"high"`
	Medium   int    `json:"medium"`
	Low      int    `json:"low"`
	// VulnsHigh counts high and critical dependency advisories.
	VulnsHigh int `json:"vulns_high"`
}

// BranchTrend is the security grade and finding counts of one branch over
// its snapshots, oldest first, and what got worse or better at its latest
// sync.
type BranchTrend struct {
	Branch       string          `json:"branch"`
	Points       []SecurityPoint `json:"points"`
	Regressions  []Change        `json:"regressions"`
	Improvements []Change        `json:"improvements"`
	Since        string          `json:"since,omitempty"`
}

// Latest returns the newest snapshot of snaps (oldest first) and the one
// before it on the same branch. ok is false when that branch has only one.
// Comparing across branches would call a branch switch a regression.
func Latest(snaps []Snapshot) (prev, cur Snapshot, ok bool) {
	if len(snaps) == 0 {
		return Snapshot{}, Snapshot{}, false
	}
	cur = snaps[len(snaps)-1]
	for i := len(snaps) - 2; i >= 0; i-- {
		if snaps[i].Branch == cur.Branch {
			return snaps[i], cur, true
		}
	}
	return Snapshot{}, cur, false
}

// GradeWorse reports a step down of the security grade from prev to cur. No
// grade before means nothing to compare with; a repository that stops being
// checkable counts as worse.
func GradeWorse(prev, cur Snapshot) bool {
	return prev.Grade != "" && rankOf(cur.Grade) > rankOf(prev.Grade)
}

// Compare lists what got worse and better from prev to cur. Metrics whose
// threshold is not reached are left out, and the security grade is compared
// on its own because it is a letter.
func Compare(prev, cur Snapshot) (regressions, improvements []Change) {
	reg, imp := []Change{}, []Change{}
	if a, b := rankOf(prev.Grade), rankOf(cur.Grade); a != b && prev.Grade != "" {
		c := Change{Key: "grade", Label: "Security grade", From: float64(a), To: float64(b),
			Detail: fmt.Sprintf("Security grade %s to %s", prev.Grade, cur.Grade)}
		if b > a {
			reg = append(reg, c)
		} else {
			imp = append(imp, c)
		}
	}
	for _, m := range metrics {
		from, to := m.get(prev), m.get(cur)
		d := to - from
		if math.Abs(d) < m.threshold {
			continue
		}
		c := Change{Key: m.key, Label: m.label, From: from, To: to, Detail: describe(m, from, to)}
		if (d > 0) == m.lowerIsBetter {
			reg = append(reg, c)
		} else {
			imp = append(imp, c)
		}
	}
	return reg, imp
}

func describe(m metric, from, to float64) string {
	f := func(v float64) string {
		if m.unit == "%" {
			return fmt.Sprintf("%.1f%%", v)
		}
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%s %s to %s", m.label, f(from), f(to))
}

// Build orders snapshots by time and compares the latest with the one before
// it on the same branch.
func Build(snaps []Snapshot) Trend {
	snaps = slices.Clone(snaps)
	slices.SortFunc(snaps, func(a, b Snapshot) int { return cmp.Compare(a.At, b.At) })
	t := Trend{Snapshots: snaps, Series: []Series{}, Regressions: []Change{}, Improvements: []Change{}, Branches: branches(snaps)}
	if t.Snapshots == nil {
		t.Snapshots = []Snapshot{}
	}
	for _, m := range metrics {
		s := Series{Key: m.key, Label: m.label, Unit: m.unit, LowerIsBetter: m.lowerIsBetter, Values: make([]float64, len(snaps))}
		for i, snap := range snaps {
			s.Values[i] = math.Round(m.get(snap)*10) / 10
		}
		t.Series = append(t.Series, s)
	}
	if prev, cur, ok := Latest(snaps); ok {
		t.Regressions, t.Improvements = Compare(prev, cur)
		t.Since = prev.At
	}
	return t
}

// branches splits snaps (oldest first) by branch.
func branches(snaps []Snapshot) []BranchTrend {
	idx := map[string]int{}
	out := []BranchTrend{}
	byBranch := [][]Snapshot{}
	for _, s := range snaps {
		i, ok := idx[s.Branch]
		if !ok {
			i = len(out)
			idx[s.Branch] = i
			out = append(out, BranchTrend{Branch: s.Branch, Points: []SecurityPoint{}, Regressions: []Change{}, Improvements: []Change{}})
			byBranch = append(byBranch, nil)
		}
		byBranch[i] = append(byBranch[i], s)
		out[i].Points = append(out[i].Points, SecurityPoint{At: s.At, Head: s.Head, Grade: s.Grade, Score: s.Score,
			Critical: s.Findings["critical"], High: s.Findings["high"], Medium: s.Findings["medium"], Low: s.Findings["low"],
			VulnsHigh: sev(s.Vulns, "high", "critical")})
	}
	for i := range out {
		if prev, cur, ok := Latest(byBranch[i]); ok {
			out[i].Regressions, out[i].Improvements = Compare(prev, cur)
			out[i].Since = prev.At
		}
	}
	latest := ""
	if len(snaps) > 0 {
		latest = snaps[len(snaps)-1].Branch
	}
	slices.SortStableFunc(out, func(a, b BranchTrend) int {
		return cmp.Or(cmp.Compare(b2i(b.Branch == latest), b2i(a.Branch == latest)), cmp.Compare(a.Branch, b.Branch))
	})
	return out
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
