package hygiene

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/astrona-io/codesynapse/internal/license"
)

// OpenSSF Scorecard scores come from deps.dev, which already answers the
// licence and version lookups: the source project of a package version
// (an ID deps.dev itself returned), then that project's Scorecard. Nothing
// but ecosystem, package name, version and project ID is sent.

const (
	// LowScorecard is the overall score below which a dependency's project
	// is a hygiene note. It is never a vulnerability.
	LowScorecard = 4.0
	// MaxScorecardChecks is how many of the lowest checks are kept.
	MaxScorecardChecks = 3
)

// scorecardChecks are the Scorecard check names kept; any other name the
// API sends is dropped, so no free text from it reaches the graph.
var scorecardChecks = map[string]bool{
	"Binary-Artifacts": true, "Branch-Protection": true, "CI-Tests": true, "CII-Best-Practices": true,
	"Code-Review": true, "Contributors": true, "Dangerous-Workflow": true, "Dependency-Update-Tool": true,
	"Fuzzing": true, "License": true, "Maintained": true, "Packaging": true, "Pinned-Dependencies": true,
	"SAST": true, "SBOM": true, "Security-Policy": true, "Signed-Releases": true, "Token-Permissions": true,
	"Vulnerabilities": true, "Webhooks": true,
}

// projectID is the shape of a deps.dev project key on the hosts it scores.
var projectID = regexp.MustCompile(`^(github\.com|gitlab\.com|bitbucket\.org)/[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)

// Score is the validated Scorecard of a dependency's source project.
type Score struct {
	// Overall is the weighted score from 0 to 10, one decimal.
	Overall float64
	// Date is the day the Scorecard was produced (YYYY-MM-DD).
	Date string
	// Lowest are up to MaxScorecardChecks checks with the lowest scores,
	// as "Name:score", lowest first.
	Lowest []string
}

// ScoreCheck is one entry of Score.Lowest.
type ScoreCheck struct {
	Name  string
	Score int
}

// ParseCheck splits a "Name:score" entry of Score.Lowest; false when the
// entry is not one.
func ParseCheck(s string) (ScoreCheck, bool) {
	name, n, ok := strings.Cut(s, ":")
	if !ok || !scorecardChecks[name] {
		return ScoreCheck{}, false
	}
	v, err := strconv.Atoi(n)
	if err != nil || v < 0 || v > 10 {
		return ScoreCheck{}, false
	}
	return ScoreCheck{Name: name, Score: v}, true
}

// ScoreOf validates a Scorecard from deps.dev: the overall score must be
// within 0 to 10 and dated, checks keep only known names and scores that
// ran. False when nothing usable is left.
func ScoreOf(sc *license.Scorecard) (Score, bool) {
	if sc == nil || sc.Date.IsZero() || math.IsNaN(sc.Overall) || sc.Overall < 0 || sc.Overall > 10 {
		return Score{}, false
	}
	var checks []ScoreCheck
	seen := map[string]bool{}
	for _, c := range sc.Checks {
		if !scorecardChecks[c.Name] || seen[c.Name] || math.IsNaN(c.Score) || c.Score < 0 || c.Score > 10 {
			continue
		}
		seen[c.Name] = true
		checks = append(checks, ScoreCheck{Name: c.Name, Score: int(math.Round(c.Score))})
	}
	slices.SortFunc(checks, func(a, b ScoreCheck) int {
		return cmp.Or(cmp.Compare(a.Score, b.Score), cmp.Compare(a.Name, b.Name))
	})
	s := Score{Overall: math.Round(sc.Overall*10) / 10, Date: sc.Date.UTC().Format("2006-01-02")}
	for _, c := range checks[:min(len(checks), MaxScorecardChecks)] {
		s.Lowest = append(s.Lowest, fmt.Sprintf("%s:%d", c.Name, c.Score))
	}
	return s, true
}

// Scorecards looks up the OpenSSF Scorecard of the source project of each
// package among deps, keyed by license.Dep.Key. Each package is asked for
// once (with its first listed version) and each project once, so packages
// sharing a repository share one request. Packages without a project, a
// project deps.dev has no Scorecard for and failed requests are left out.
func Scorecards(ctx context.Context, c *license.DepsDev, deps []license.Dep) map[string]Score {
	byPackage := map[[2]string][]license.Dep{}
	var order [][2]string
	for _, d := range deps {
		k := [2]string{d.Ecosystem, d.Name}
		if _, ok := byPackage[k]; !ok {
			order = append(order, k)
		}
		byPackage[k] = append(byPackage[k], d)
	}
	var mu sync.Mutex
	project := map[[2]string]string{}
	each(ctx, order, func(k [2]string) {
		id, err := c.SourceProject(ctx, k[0], k[1], byPackage[k][0].Version)
		if err != nil || !projectID.MatchString(id) {
			return
		}
		mu.Lock()
		project[k] = id
		mu.Unlock()
	})
	var projects []string
	seen := map[string]bool{}
	for _, k := range order {
		if id, ok := project[k]; ok && !seen[id] {
			seen[id] = true
			projects = append(projects, id)
		}
	}
	scores := map[string]Score{}
	each(ctx, projects, func(id string) {
		sc, err := c.Scorecard(ctx, id)
		if err != nil {
			return
		}
		if s, ok := ScoreOf(sc); ok {
			mu.Lock()
			scores[id] = s
			mu.Unlock()
		}
	})
	out := map[string]Score{}
	for k, id := range project {
		if s, ok := scores[id]; ok {
			for _, d := range byPackage[k] {
				out[d.Key()] = s
			}
		}
	}
	return out
}
