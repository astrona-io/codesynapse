package review

import (
	"slices"

	"github.com/astrona-io/codesynapse/internal/runtimefacts"
)

// Measured is what the last recorded test run (coverage profiles and JUnit
// results; internal/runtimefacts) says about a change, next to the graph's
// static test reach.
type Measured struct {
	// At is the commit measured, or the time for local reports.
	At string `json:"at"`
	// Stale is set when the run measured another commit than the graph.
	Stale bool `json:"stale,omitempty"`
	// Coverage lists changed functions with their measured coverage.
	Coverage []string `json:"coverage,omitempty"`
	// Tests lists tests reaching the change that were flaky or failing.
	Tests []string `json:"tests,omitempty"`
}

// MeasuredOf summarises f for the changed symbols of r; nil when the run
// measured none of them.
func MeasuredOf(f runtimefacts.Facts, stale bool, r Report) *Measured {
	out := &Measured{At: f.At(), Stale: stale}
	for _, c := range r.Changes {
		if note, ok := f.CoverageNote(c.ID); ok {
			out.Coverage = append(out.Coverage, c.Name+": "+note)
		}
		for _, t := range c.Tests {
			if note, ok := f.RunNote(t.ID); ok {
				if line := t.Name + ": " + note; !slices.Contains(out.Tests, line) {
					out.Tests = append(out.Tests, line)
				}
			}
		}
	}
	if len(out.Coverage) == 0 && len(out.Tests) == 0 {
		return nil
	}
	return out
}
