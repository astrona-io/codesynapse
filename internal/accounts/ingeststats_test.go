package accounts

import (
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestAddIngestStats(t *testing.T) {
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	f := IngestFigures{Succeeded: 3, Failed: 1, Cancelled: 1, Running: 4, WithoutReport: 1, IncompleteDiscovery: 1,
		Scanned: 100, Indexed: 80, Skipped: 15, FilesFailed: 2, Unprocessed: 3,
		Skips: IngestSkips{IgnoredPath: 7, GeneratedCode: 4, Binary: 3, NonK8sYAML: 1}}
	set := NewPlatformStatSet(day)
	AddIngestStats(set, []OrgIngestStats{
		{OrgID: "org-a", OrgExists: true, IngestFigures: f},
		{OrgID: "org-gone", IngestFigures: f},
	})
	total, org := map[[2]string]int{}, map[[3]string]int{}
	for _, r := range set.Rows() {
		if r.OrgID == "" {
			total[[2]string{r.Metric, r.Dimension}] += int(r.Value)
		} else {
			org[[3]string{r.OrgID, r.Metric, r.Dimension}] += int(r.Value)
		}
	}
	got := IngestFiguresOf(func(m, d string) int { return total[[2]string{m, d}] })
	want := f
	want.Add(f)
	want.Running = 0
	assert.Equal(t, got, want)
	got = IngestFiguresOf(func(m, d string) int { return org[[3]string{"org-a", m, d}] })
	want = f
	want.Running = 0
	assert.Equal(t, got, want)
	for k := range org {
		assert.NotEqual(t, k[0], "org-gone")
	}
}

func TestIngestInWindow(t *testing.T) {
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 1)
	before, inside, after := from.Add(-time.Hour), from.Add(time.Hour), to.Add(time.Hour)
	for _, c := range []struct {
		name     string
		terminal bool
		started  time.Time
		finished *time.Time
		want     bool
	}{
		{"running since before", false, before, nil, true},
		{"running from after", false, after, nil, false},
		{"finished inside", true, before, &inside, true},
		{"finished before", true, before, &before, false},
		{"finished at the end", true, inside, &to, false},
		{"expired, started inside", true, inside, nil, true},
		{"expired, started before", true, before, nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, IngestInWindow(c.terminal, c.started, c.finished, from, to), c.want)
		})
	}
}
