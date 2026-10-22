package history

import (
	"fmt"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func snap(at string, f func(*Snapshot)) Snapshot {
	s := Snapshot{At: at, Grade: "A", Score: 95, Findings: map[string]int{}, Vulns: map[string]int{}, TestReachPct: 60, DocCoveragePct: 40, DupPct: 2}
	if f != nil {
		f(&s)
	}
	return s
}

func keys(cs []Change) []string {
	out := []string{}
	for _, c := range cs {
		out = append(out, c.Key)
	}
	return out
}

func TestCompareFindsWhatMatters(t *testing.T) {
	prev := snap("2026-10-01T00:00:00Z", nil)
	cur := snap("2026-10-02T00:00:00Z", func(s *Snapshot) {
		s.Grade, s.Score = "C", 70
		s.VulnsCalled = 2
		s.Findings = map[string]int{"critical": 1, "low": 30}
		s.DupPct = 3.1
		s.TestReachPct = 55
		s.DeadFunctions = 3 // below the threshold of 5
		s.DocCoveragePct = 41
	})
	reg, imp := Compare(prev, cur)
	assert.Equal(t, keys(reg), []string{"grade", "score", "vulns_called", "findings_high", "dup_pct", "test_reach_pct"})
	assert.Equal(t, len(imp), 0)
	assert.Equal(t, reg[0].Detail, "Security grade A to C")
	assert.Equal(t, reg[4].Detail, "Duplicate code 2.0% to 3.1%")

	reg, imp = Compare(cur, prev)
	assert.Equal(t, len(reg), 0)
	assert.Equal(t, keys(imp), []string{"grade", "score", "vulns_called", "findings_high", "dup_pct", "test_reach_pct"})
}

func TestCompareIgnoresNoiseAndSize(t *testing.T) {
	prev := snap("1", nil)
	cur := snap("2", func(s *Snapshot) { s.Modules, s.FunctionLOC, s.DupPct, s.DocCoveragePct = 900, 100000, 2.3, 42 })
	reg, imp := Compare(prev, cur)
	assert.Equal(t, len(reg)+len(imp), 0, "growth and small moves are not regressions")

	// A repository that stops being checkable is a change, not a clean bill.
	reg, _ = Compare(snap("1", nil), snap("2", func(s *Snapshot) { s.Grade, s.Score = "N/A", 0 }))
	assert.Contains(t, keys(reg), "grade")
	// No grade before means nothing to compare it with.
	reg, _ = Compare(snap("1", func(s *Snapshot) { s.Grade = "" }), snap("2", nil))
	assert.False(t, slicesContains(keys(reg), "grade"))
}

func slicesContains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestBuildOrdersAndSeries(t *testing.T) {
	out := Build([]Snapshot{
		snap("2026-10-03T00:00:00Z", func(s *Snapshot) { s.VulnsCalled = 1 }),
		snap("2026-10-01T00:00:00Z", nil),
		snap("2026-10-02T00:00:00Z", nil),
	})
	require.Len(t, out.Snapshots, 3)
	assert.Equal(t, out.Snapshots[0].At, "2026-10-01T00:00:00Z", "oldest first")
	assert.Equal(t, out.Since, "2026-10-02T00:00:00Z")
	assert.Equal(t, keys(out.Regressions), []string{"vulns_called"})
	var called Series
	for _, s := range out.Series {
		if s.Key == "vulns_called" {
			called = s
		}
	}
	assert.Equal(t, called.Values, []float64{0, 0, 1})
	assert.True(t, called.LowerIsBetter)

	one := Build([]Snapshot{snap("1", nil)})
	assert.Equal(t, len(one.Regressions), 0)
	assert.Equal(t, one.Since, "")
	empty := Build(nil)
	assert.True(t, empty.Snapshots != nil && empty.Regressions != nil && empty.Improvements != nil && empty.Series != nil, "empty lists encode as [] not null")
	assert.Equal(t, fmt.Sprint(len(empty.Series)), fmt.Sprint(len(metrics)))
}
