// Package runtimefacts turns coverage profiles and JUnit test results into
// facts about the graph's functions and tests (docs/PLAN.md, agent workflow
// roadmap step 11): how much of a function a test run executed, and how
// each test ended, so "tested" can mean measured rather than statically
// reachable, and a flaky test is known as one before an agent chases it.
//
// Facts are kept apart from the code graph, keyed by node ID and pinned to
// the commit they were measured at: a re-index replaces the graph's nodes,
// not what a test run measured, and answers say when the two differ. They
// hold counts only, never source.
package runtimefacts

import (
	"cmp"
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/model"
)

// Facts are the measured facts of one repository.
type Facts struct {
	// Commit is the commit the reports were measured at, when known.
	Commit     string    `json:"commit,omitempty"`
	MeasuredAt time.Time `json:"measured_at"`
	// Origin is "local" (files in the checkout) or "upload" (from CI).
	Origin string `json:"origin"`
	// Sources name the reports, e.g. "go coverage cover.out" (base names only).
	Sources  []string            `json:"sources"`
	Unit     string              `json:"unit,omitempty"`
	Coverage map[string]Coverage `json:"coverage"`
	Tests    map[string]TestRun  `json:"tests"`
	// Unmatched counts report files and test cases no indexed file or test
	// matched (vendored code, generated files, other modules).
	UnmatchedFiles int `json:"unmatched_files,omitempty"`
	UnmatchedTests int `json:"unmatched_tests,omitempty"`
}

// Coverage is how much of one function a run executed.
type Coverage struct {
	Covered int `json:"covered"`
	Total   int `json:"total"`
}

// Percent is the covered share, 0–100.
func (c Coverage) Percent() int {
	if c.Total == 0 {
		return 0
	}
	return c.Covered * 100 / c.Total
}

// TestRun is how one test ended across the reports read.
type TestRun struct {
	Passed  int `json:"passed,omitempty"`
	Failed  int `json:"failed,omitempty"`
	Skipped int `json:"skipped,omitempty"`
	Millis  int `json:"millis,omitempty"`
}

// Flaky reports a test that both passed and failed: a retry in one run
// (gotestsum --rerun-fails, pytest-rerunfailures) or two reports.
func (t TestRun) Flaky() bool { return t.Passed > 0 && t.Failed > 0 }

// Status is the one word an answer shows for the test.
func (t TestRun) Status() string {
	switch {
	case t.Flaky():
		return "flaky"
	case t.Failed > 0:
		return "failed"
	case t.Passed > 0:
		return "passed"
	case t.Skipped > 0:
		return "skipped"
	}
	return ""
}

// Source is the graph a report is mapped onto. It is asked by path, so a
// store needs no listing of every file.
type Source interface {
	// HasFile reports whether file (repository-relative) is indexed.
	HasFile(ctx context.Context, file string) (bool, error)
	// FilesIn lists the indexed files directly in dir ("." for the root).
	FilesIn(ctx context.Context, dir string) ([]string, error)
	// InFile lists the functions, methods and tests declared in file.
	InFile(ctx context.Context, file string) ([]model.Node, error)
}

// Input is what one mapping reads.
type Input struct {
	Profiles []Profile
	Tests    []TestCase
}

// Map attributes coverage blocks to the functions that contain them and
// test cases to test nodes.
func Map(ctx context.Context, src Source, in Input) (Facts, error) {
	m := mapper{src: src, files: map[string]bool{}, dirs: map[string][]string{}, nodes: map[string][]model.Node{}, found: map[string]string{}}
	out := Facts{Coverage: map[string]Coverage{}, Tests: map[string]TestRun{}, Sources: []string{}}

	unmatched := map[string]bool{}
	for _, p := range in.Profiles {
		if out.Unit == "" {
			out.Unit = p.Unit
		}
		byFile := map[string][]Block{}
		for _, b := range p.Blocks {
			f, err := m.file(ctx, b.File)
			if err != nil {
				return Facts{}, err
			}
			if f == "" {
				unmatched[b.File] = true
				continue
			}
			byFile[f] = append(byFile[f], b)
		}
		for f, blocks := range byFile {
			nodes, err := m.inFile(ctx, f)
			if err != nil {
				return Facts{}, err
			}
			for _, n := range nodes {
				if !n.Kind.IsCallable() || n.StartLine == 0 || n.IsTest {
					continue
				}
				c := out.Coverage[n.ID]
				for _, b := range blocks {
					if b.StartLine >= n.StartLine && b.EndLine <= max(n.EndLine, n.StartLine) {
						c.Total += b.Units
						if b.Hit {
							c.Covered += b.Units
						}
					}
				}
				if c.Total > 0 {
					out.Coverage[n.ID] = c
				}
			}
		}
	}
	out.UnmatchedFiles = len(unmatched)

	for _, tc := range in.Tests {
		n, err := m.test(ctx, tc)
		if err != nil {
			return Facts{}, err
		}
		if n == "" {
			out.UnmatchedTests++
			continue
		}
		run := out.Tests[n]
		switch tc.Outcome {
		case Passed:
			run.Passed++
		case Failed:
			run.Failed++
		case Skipped:
			run.Skipped++
		}
		run.Millis += tc.Millis
		out.Tests[n] = run
	}
	return out, nil
}

type mapper struct {
	src   Source
	files map[string]bool     // path -> indexed
	dirs  map[string][]string // directory -> indexed files in it
	nodes map[string][]model.Node
	found map[string]string // reported path -> indexed file ("" for none)
}

// file finds the indexed file a reported path names: the path itself, or
// its longest suffix that is an indexed path. Go profiles name files by
// import path and LCOV often by absolute path; both end in the
// repository-relative one.
func (m mapper) file(ctx context.Context, reported string) (string, error) {
	if f, ok := m.found[reported]; ok {
		return f, nil
	}
	f, err := m.longest(reported, func(p string) (bool, error) {
		if ok, seen := m.files[p]; seen {
			return ok, nil
		}
		ok, err := m.src.HasFile(ctx, p)
		m.files[p] = ok
		return ok, err
	})
	m.found[reported] = f
	return f, err
}

// dir is the same for a directory (a Go package path).
func (m mapper) dir(ctx context.Context, reported string) (string, error) {
	return m.longest(reported, func(p string) (bool, error) {
		files, err := m.filesIn(ctx, p)
		return len(files) > 0, err
	})
}

func (m mapper) filesIn(ctx context.Context, dir string) ([]string, error) {
	if files, ok := m.dirs[dir]; ok {
		return files, nil
	}
	files, err := m.src.FilesIn(ctx, dir)
	m.dirs[dir] = files
	return files, err
}

// longest drops leading segments of p until exists says yes.
func (m mapper) longest(p string, exists func(string) (bool, error)) (string, error) {
	for p != "" {
		ok, err := exists(p)
		if err != nil {
			return "", err
		}
		if ok {
			return p, nil
		}
		_, rest, cut := strings.Cut(p, "/")
		if !cut {
			return "", nil
		}
		p = rest
	}
	return "", nil
}

func (m mapper) inFile(ctx context.Context, file string) ([]model.Node, error) {
	if n, ok := m.nodes[file]; ok {
		return n, nil
	}
	n, err := m.src.InFile(ctx, file)
	if err != nil {
		return nil, err
	}
	m.nodes[file] = n
	return n, nil
}

// test finds the test node a test case reports on: in the file the report
// names, else in the Go package or Python module the suite names. Subtests
// and parametrised cases count for their test function.
func (m mapper) test(ctx context.Context, tc TestCase) (string, error) {
	name, _, _ := strings.Cut(tc.Name, "/")
	name, _, _ = strings.Cut(name, "[")
	name = strings.TrimSpace(name)
	var candidates []string
	if tc.File != "" {
		f, err := m.file(ctx, tc.File)
		if err != nil {
			return "", err
		}
		if f != "" {
			candidates = append(candidates, f)
		}
	}
	if len(candidates) == 0 && tc.Suite != "" {
		suite := strings.ReplaceAll(tc.Suite, "\\", "/")
		d, err := m.dir(ctx, suite)
		if err != nil {
			return "", err
		}
		if d == "" && strings.Contains(suite, "/") {
			if root, err := m.filesIn(ctx, "."); err != nil {
				return "", err
			} else if len(root) > 0 {
				d = "." // the module's root package: its path is the module path
			}
		}
		if d != "" && strings.Contains(suite, "/") {
			for _, f := range m.dirs[d] {
				if strings.HasSuffix(f, "_test.go") {
					candidates = append(candidates, f)
				}
			}
		} else {
			// pytest: "tests.test_app" or "tests.test_app.TestClass".
			parts := strings.Split(tc.Suite, ".")
			for i := len(parts); i > 0 && len(candidates) == 0; i-- {
				f, err := m.file(ctx, strings.Join(parts[:i], "/")+".py")
				if err != nil {
					return "", err
				}
				if f != "" {
					candidates = append(candidates, f)
				}
			}
		}
	}
	slices.Sort(candidates)
	var found string
	for _, f := range candidates {
		nodes, err := m.inFile(ctx, f)
		if err != nil {
			return "", err
		}
		for _, n := range nodes {
			if n.IsTest && n.Kind.IsCallable() && n.Name == name {
				if found != "" && found != n.ID {
					return "", nil // ambiguous: count it as unmatched
				}
				found = n.ID
			}
		}
	}
	return found, nil
}

// NodeSource is a Source over nodes held in memory (an ingest result).
func NodeSource(nodes []model.Node) Source {
	s := nodeSource{files: map[string]bool{}, dirs: map[string][]string{}, byFile: map[string][]model.Node{}}
	for _, n := range nodes {
		switch {
		case n.Kind == model.KindFile:
			s.files[n.File] = true
			s.dirs[path.Dir(n.File)] = append(s.dirs[path.Dir(n.File)], n.File)
		case n.Kind.IsCallable() && n.File != "":
			s.byFile[n.File] = append(s.byFile[n.File], n)
		}
	}
	for _, files := range s.dirs {
		slices.Sort(files)
	}
	return s
}

type nodeSource struct {
	files  map[string]bool
	dirs   map[string][]string
	byFile map[string][]model.Node
}

func (s nodeSource) HasFile(_ context.Context, f string) (bool, error) { return s.files[f], nil }
func (s nodeSource) FilesIn(_ context.Context, d string) ([]string, error) {
	return s.dirs[d], nil
}
func (s nodeSource) InFile(_ context.Context, f string) ([]model.Node, error) {
	return s.byFile[f], nil
}

// At names when the facts were measured: the commit (short), else the time.
func (f Facts) At() string {
	if f.Commit != "" {
		return f.Commit[:min(len(f.Commit), 12)]
	}
	if !f.MeasuredAt.IsZero() {
		return f.MeasuredAt.UTC().Format(time.RFC3339)
	}
	return "unknown"
}

// StaleFor reports facts measured at another commit than revision.
func (f Facts) StaleFor(revision string) bool {
	if f.Commit == "" || revision == "" {
		return false
	}
	return !strings.HasPrefix(f.Commit, revision) && !strings.HasPrefix(revision, f.Commit)
}

// CoverageNote says what the run measured of function id, e.g.
// "2 of 3 statements covered (66%)".
func (f Facts) CoverageNote(id string) (string, bool) {
	c, ok := f.Coverage[id]
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%d of %d %s covered (%d%%)", c.Covered, c.Total, cmp.Or(f.Unit, "statements"), c.Percent()), true
}

// RunNote says how test id ended when that matters to an agent: flaky or
// failed. Passing and skipped tests get no note.
func (f Facts) RunNote(id string) (string, bool) {
	t, ok := f.Tests[id]
	if !ok {
		return "", false
	}
	switch {
	case t.Flaky():
		return fmt.Sprintf("flaky in the last recorded run (%d passed, %d failed): a failure may not be yours", t.Passed, t.Failed), true
	case t.Failed > 0:
		return "failed in the last recorded run", true
	}
	return "", false
}
