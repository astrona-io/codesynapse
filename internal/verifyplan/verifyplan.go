// Package verifyplan turns a change into the commands that check it
// (docs/PLAN.md, agent workflow roadmap step 7): the tests that reach the
// changed code, closest first, then build and lint for each language
// touched. Agents lose turns running everything, or the wrong thing; the
// graph already knows which tests call what changed.
package verifyplan

import (
	"cmp"
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/review"
	"github.com/astrona-io/codesynapse/internal/store"
)

// Step is one command to run.
type Step struct {
	// Kind is test, build or lint.
	Kind    string `json:"kind"`
	Command string `json:"command"`
	Why     string `json:"why"`
	// Reach is how many calls separate the closest test from the change:
	// 0 when the change is to the test itself, 1 for a direct caller.
	Reach int      `json:"reach,omitempty"`
	Tests []string `json:"tests,omitempty"`
}

// Plan is what to run, in order.
type Plan struct {
	Steps []Step `json:"steps"`
	// Untested names changed functions no test reaches: check them by hand
	// or add a test.
	Untested []string `json:"untested,omitempty"`
	// Notes include what the last recorded test run measured: coverage of
	// untested functions and tests that were flaky or failing.
	Notes []string `json:"notes,omitempty"`
}

// Limits keep commands readable and answers small.
const (
	maxTestsPerCommand = 40
	maxTestSteps       = 12
)

// group is the tests one command runs.
type group struct {
	kind, key string // language and package directory or file
	reach     int
	covers    map[string]bool // changed symbols it reaches
	tests     []string
}

// Build plans the checks for a change analysed against the head graph r.
func Build(ctx context.Context, r store.Reader, repo string, rep review.Report) (Plan, error) {
	groups := map[string]*group{}
	add := func(n model.Node, reach int, changed string) {
		lang, key, name := testTarget(n)
		if key == "" {
			return
		}
		g := groups[lang+"\x00"+key]
		if g == nil {
			g = &group{kind: lang, key: key, reach: reach, covers: map[string]bool{}}
			groups[lang+"\x00"+key] = g
		}
		g.reach = min(g.reach, reach)
		g.covers[changed] = true
		if !slices.Contains(g.tests, name) {
			g.tests = append(g.tests, name)
		}
	}
	plan := Plan{Steps: []Step{}}
	for _, c := range rep.Changes {
		n, err := r.Node(ctx, c.ID)
		if err != nil {
			continue
		}
		if n.IsTest {
			add(n, 0, c.Name)
			continue
		}
		for _, t := range c.Tests {
			tn, err := r.Node(ctx, t.ID)
			if err != nil {
				continue
			}
			add(tn, max(t.Depth, 1), c.Name)
		}
		if len(c.Tests) == 0 && n.Kind.IsCallable() {
			plan.Untested = append(plan.Untested, c.Name)
		}
	}

	ordered := make([]*group, 0, len(groups))
	for _, g := range groups {
		ordered = append(ordered, g)
	}
	slices.SortFunc(ordered, func(a, b *group) int {
		return cmp.Or(cmp.Compare(a.reach, b.reach), cmp.Compare(len(b.covers), len(a.covers)), cmp.Compare(a.key, b.key))
	})
	for i, g := range ordered {
		if i == maxTestSteps {
			plan.Notes = append(plan.Notes, fmt.Sprintf("%d more test groups reach the change more distantly; run the suite before merging", len(ordered)-i))
			break
		}
		slices.Sort(g.tests)
		plan.Steps = append(plan.Steps, testStep(g))
	}

	checks, err := buildAndLint(ctx, r, repo, rep.Files)
	if err != nil {
		return Plan{}, err
	}
	plan.Steps = append(plan.Steps, checks...)
	if len(ordered) == 0 {
		plan.Notes = append(plan.Notes, "no indexed test reaches the change")
	}
	notes, err := measuredNotes(ctx, r, repo, rep)
	if err != nil {
		return Plan{}, err
	}
	plan.Notes = append(plan.Notes, notes...)
	return plan, nil
}

// testTarget is how a test is run: its language, what one command covers
// (a Go package directory, a Python or JavaScript file) and its name there.
func testTarget(n model.Node) (lang, key, name string) {
	_, local, _ := strings.Cut(strings.TrimPrefix(n.ID, n.Repo+"|"), "|")
	switch ext := path.Ext(n.File); {
	case ext == ".go":
		if !strings.HasPrefix(n.Name, "Test") && !strings.HasPrefix(n.Name, "Fuzz") && !strings.HasPrefix(n.Name, "Benchmark") && !strings.HasPrefix(n.Name, "Example") {
			return "", "", "" // a helper in a test file: its callers are the tests
		}
		return "go", path.Dir(n.File), n.Name
	case ext == ".py":
		_, sym, ok := strings.Cut(local, "::")
		if !ok {
			return "", "", ""
		}
		return "python", n.File, strings.ReplaceAll(sym, ".", "::")
	case slices.Contains([]string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".vue"}, ext):
		return "javascript", n.File, n.Name
	}
	return "", "", ""
}

func testStep(g *group) Step {
	tests := g.tests[:min(len(g.tests), maxTestsPerCommand)]
	s := Step{Kind: "test", Reach: g.reach, Tests: tests}
	covers := make([]string, 0, len(g.covers))
	for c := range g.covers {
		covers = append(covers, c)
	}
	slices.Sort(covers)
	named := strings.Join(covers[:min(len(covers), 3)], ", ")
	if len(covers) > 3 {
		named += fmt.Sprintf(" and %d more", len(covers)-3)
	}
	switch g.kind {
	case "go":
		s.Command = fmt.Sprintf("go test ./%s -run '^(%s)$'", g.key, strings.Join(tests, "|"))
	case "python":
		ids := make([]string, 0, len(tests))
		for _, t := range tests {
			ids = append(ids, g.key+"::"+t)
		}
		s.Command = "pytest " + strings.Join(ids, " ")
	case "javascript":
		// The package's own test script, filtered to the file: jest and
		// vitest both take file filters.
		s.Command = "npm test -- " + g.key
	}
	switch g.reach {
	case 0:
		s.Why = "the change edits these tests"
	case 1:
		s.Why = "call " + named + " directly"
	default:
		s.Why = fmt.Sprintf("reach %s through %d calls", named, g.reach)
	}
	return s
}

// buildAndLint lists the build and lint commands for the languages of the
// changed files.
func buildAndLint(ctx context.Context, r store.Reader, repo string, files []review.FileSummary) ([]Step, error) {
	var goDirs, pyFiles []string
	for _, f := range files {
		if f.Status == "deleted" {
			continue
		}
		switch path.Ext(f.Path) {
		case ".go":
			if d := "./" + path.Dir(f.Path); !slices.Contains(goDirs, d) {
				goDirs = append(goDirs, d)
			}
		case ".py":
			pyFiles = append(pyFiles, f.Path)
		}
	}
	slices.Sort(goDirs)
	var steps []Step
	if len(goDirs) > 0 {
		pkgs := strings.Join(goDirs, " ")
		steps = append(steps,
			Step{Kind: "build", Command: "go build " + pkgs, Why: "the changed packages compile"},
			Step{Kind: "lint", Command: "go vet " + pkgs, Why: "go vet on the changed packages"})
		if ok, err := hasFile(ctx, r, repo, ".golangci"); err != nil {
			return nil, err
		} else if ok {
			steps = append(steps, Step{Kind: "lint", Command: "golangci-lint run " + strings.ReplaceAll(pkgs, " ", "/... ") + "/...", Why: "the repository's golangci-lint configuration"})
		}
	}
	if len(pyFiles) > 0 {
		steps = append(steps, Step{Kind: "build", Command: "python -m py_compile " + strings.Join(pyFiles, " "), Why: "the changed Python files parse"})
	}
	return steps, nil
}

// hasFile reports whether the graph knows a top-level file named prefix…
// (a configuration file the repository keeps at its root).
func hasFile(ctx context.Context, r store.Reader, repo, prefix string) (bool, error) {
	nodes, err := r.Search(ctx, repo, prefix, []model.NodeKind{model.KindConfig, model.KindFile}, 5)
	if err != nil {
		return false, err
	}
	for _, n := range nodes {
		if strings.HasPrefix(n.File, prefix) {
			return true, nil
		}
	}
	return false, nil
}

// measuredNotes adds what the last recorded test run (internal/runtimefacts)
// says: a function the graph sees no test reach may still be covered, and
// a test reaching it that was flaky or failing before should not be blamed
// on the change.
func measuredNotes(ctx context.Context, r store.Reader, repo string, rep review.Report) ([]string, error) {
	f, ok, err := store.RuntimeFactsOf(ctx, r, repo)
	if err != nil || !ok {
		return nil, err
	}
	var notes []string
	seen := map[string]bool{}
	for _, c := range rep.Changes {
		if len(c.Tests) == 0 {
			if note, ok := f.CoverageNote(c.ID); ok {
				notes = append(notes, fmt.Sprintf("%s: no test reaches it in the graph, but the run at %s measured %s", c.Name, f.At(), note))
			}
		}
		for _, t := range c.Tests {
			if note, ok := f.RunNote(t.ID); ok && !seen[t.ID] {
				seen[t.ID] = true
				notes = append(notes, t.Name+": "+note)
			}
		}
	}
	return notes, nil
}
