package api

import (
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestNextStatsRollup(t *testing.T) {
	day := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		now  time.Time
		want time.Duration
	}{
		{name: "midnight", now: day, want: 5 * time.Minute},
		{name: "just before", now: day.Add(4 * time.Minute), want: time.Minute},
		{name: "at the time", now: day.Add(5 * time.Minute), want: 24 * time.Hour},
		{name: "afternoon", now: day.Add(14 * time.Hour), want: 10*time.Hour + 5*time.Minute},
		{name: "another zone", now: day.Add(23 * time.Hour).In(time.FixedZone("CET", 3600)), want: time.Hour + 5*time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, nextStatsRollup(c.now), c.want)
		})
	}
}
