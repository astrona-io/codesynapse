package history

import (
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func onBranch(at, branch string, f func(*Snapshot)) Snapshot {
	return snap(at, func(s *Snapshot) {
		s.Branch = branch
		if f != nil {
			f(s)
		}
	})
}

func TestLatestComparesWithinABranch(t *testing.T) {
	tests := []struct {
		name   string
		snaps  []Snapshot
		ok     bool
		prevAt string
		curAt  string
	}{
		{"none", nil, false, "", ""},
		{"one", []Snapshot{onBranch("1", "main", nil)}, false, "", "1"},
		{"same branch", []Snapshot{onBranch("1", "main", nil), onBranch("2", "main", nil)}, true, "1", "2"},
		{"skips other branches", []Snapshot{onBranch("1", "main", nil), onBranch("2", "dev", nil), onBranch("3", "main", nil)}, true, "1", "3"},
		{"first of a branch", []Snapshot{onBranch("1", "main", nil), onBranch("2", "dev", nil)}, false, "", "2"},
		{"unknown branch matches unknown", []Snapshot{onBranch("1", "", nil), onBranch("2", "", nil)}, true, "1", "2"},
		{"unknown is not a branch", []Snapshot{onBranch("1", "", nil), onBranch("2", "main", nil)}, false, "", "2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev, cur, ok := Latest(tt.snaps)
			assert.Equal(t, ok, tt.ok)
			assert.Equal(t, prev.At, tt.prevAt)
			assert.Equal(t, cur.At, tt.curAt)
		})
	}
}

func TestGradeWorse(t *testing.T) {
	tests := []struct {
		from, to string
		want     bool
	}{
		{"A", "B", true},
		{"B", "A", false},
		{"C", "C", false},
		{"B", "N/A", true},
		{"", "F", false},
	}
	for _, tt := range tests {
		t.Run(tt.from+"->"+tt.to, func(t *testing.T) {
			assert.Equal(t, GradeWorse(Snapshot{Grade: tt.from}, Snapshot{Grade: tt.to}), tt.want)
		})
	}
}

func TestBuildPerBranch(t *testing.T) {
	out := Build([]Snapshot{
		onBranch("2026-10-01T00:00:00Z", "main", func(s *Snapshot) { s.Findings = map[string]int{"high": 1, "low": 4} }),
		onBranch("2026-10-02T00:00:00Z", "dev", func(s *Snapshot) { s.Grade, s.Score = "D", 50 }),
		onBranch("2026-10-03T00:00:00Z", "main", func(s *Snapshot) {
			s.Grade, s.Score = "B", 85
			s.Findings = map[string]int{"critical": 1, "high": 2, "medium": 3}
			s.Vulns = map[string]int{"high": 2}
		}),
	})
	// The latest main sync is compared with main, not with the dev sync
	// between them (which would read as an improvement from D).
	assert.Equal(t, out.Since, "2026-10-01T00:00:00Z")
	assert.Contains(t, keys(out.Regressions), "grade")
	assert.Equal(t, len(out.Improvements), 0)

	require.Len(t, out.Branches, 2)
	main, dev := out.Branches[0], out.Branches[1]
	assert.Equal(t, main.Branch, "main", "the branch of the latest snapshot first")
	require.Len(t, main.Points, 2)
	assert.Equal(t, main.Points[1], SecurityPoint{At: "2026-10-03T00:00:00Z", Grade: "B", Score: 85, Critical: 1, High: 2, Medium: 3, VulnsHigh: 2})
	assert.Equal(t, main.Points[0].Low, 4)
	assert.Contains(t, keys(main.Regressions), "findings_high")
	assert.Equal(t, main.Since, "2026-10-01T00:00:00Z")

	assert.Equal(t, dev.Branch, "dev")
	require.Len(t, dev.Points, 1)
	assert.Equal(t, len(dev.Regressions), 0)
	assert.Equal(t, dev.Since, "")

	assert.True(t, Build(nil).Branches != nil, "no branches encode as [] not null")
}
