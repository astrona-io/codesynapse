package runtimefacts

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Report names Discover looks for at the root of a checkout and in the
// folders tools write to by default.
var (
	coverageNames = []string{
		"cover.out", "coverage.out", "cover.prof", "coverage.txt", "coverage.prof",
		"lcov.info", "coverage/lcov.info", "coverage.lcov",
	}
	junitNames = []string{
		"junit.xml", "report.xml", "test-results.xml", "junit-report.xml",
		"test-results/junit.xml", "reports/junit.xml",
	}
)

// Files are the reports found or named for one checkout.
type Files struct {
	Coverage []string
	JUnit    []string
}

// Discover finds the coverage and JUnit reports a test run left at the
// usual places in root. It never walks the tree.
func Discover(root string) Files {
	var out Files
	find := func(names []string) []string {
		var found []string
		for _, n := range names {
			p := filepath.Join(root, filepath.FromSlash(n))
			if st, err := os.Lstat(p); err == nil && st.Mode().IsRegular() {
				found = append(found, p)
			}
		}
		return found
	}
	out.Coverage, out.JUnit = find(coverageNames), find(junitNames)
	return out
}

// Empty reports no reports.
func (f Files) Empty() bool { return len(f.Coverage) == 0 && len(f.JUnit) == 0 }

// Load reads the reports in files and maps them onto src. MeasuredAt is the
// newest report's modification time; the commit is unknown for local
// files, so answers show the time instead.
func Load(ctx context.Context, src Source, files Files) (Facts, error) {
	var in Input
	var sources []string
	var newest time.Time
	read := func(p, kind string, parse func(*os.File) error) error {
		f, err := os.Open(p) //nolint:gosec // a report the user named or left at a known place
		if err != nil {
			return err
		}
		defer f.Close() //nolint:errcheck // read-only
		if st, err := f.Stat(); err == nil {
			if st.Size() > MaxReportBytes {
				return fmt.Errorf("%s: %w", filepath.Base(p), ErrTooLarge)
			}
			if st.ModTime().After(newest) {
				newest = st.ModTime()
			}
		}
		if err := parse(f); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(p), err)
		}
		sources = append(sources, kind+" "+filepath.Base(p))
		return nil
	}
	for _, p := range files.Coverage {
		if err := read(p, "coverage", func(f *os.File) error {
			prof, err := ParseCoverage(f)
			in.Profiles = append(in.Profiles, prof)
			return err
		}); err != nil {
			return Facts{}, err
		}
	}
	for _, p := range files.JUnit {
		if err := read(p, "junit", func(f *os.File) error {
			cases, err := ParseJUnit(f)
			in.Tests = append(in.Tests, cases...)
			return err
		}); err != nil {
			return Facts{}, err
		}
	}
	facts, err := Map(ctx, src, in)
	if err != nil {
		return Facts{}, err
	}
	slices.Sort(sources)
	facts.Sources, facts.MeasuredAt, facts.Origin = sources, newest.UTC(), "local"
	return facts, nil
}
