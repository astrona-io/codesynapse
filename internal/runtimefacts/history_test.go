package runtimefacts

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

const tf, tg = "r|Function|pkg.TestF", "r|Function|pkg.TestG"

// runs uploads one outcome per run for TestF (p, f, x, s or - for absent),
// each at its commit, and returns the facts after the last.
func runs(t *testing.T, outcomes, commits string) Facts {
	t.Helper()
	require.Equal(t, len(outcomes), len(commits))
	var f Facts
	for i := range len(outcomes) {
		next := Facts{Commit: string(commits[i]), MeasuredAt: time.Unix(int64(i), 0), Tests: map[string]TestRun{tg: {Passed: 1}}}
		switch outcomes[i] {
		case 'p':
			next.Tests[tf] = TestRun{Passed: 1}
		case 'f':
			next.Tests[tf] = TestRun{Failed: 1}
		case 'x':
			next.Tests[tf] = TestRun{Passed: 1, Failed: 1}
		case 's':
			next.Tests[tf] = TestRun{Skipped: 1}
		}
		f = next.WithHistory(f)
	}
	return f
}

func TestTrend(t *testing.T) {
	for _, tt := range []struct {
		outcomes, commits string
		flaky             bool
		note              string
	}{
		{"fp", "aa", true, "flaky across runs: passed and failed at commit a (failed in 1 of the last 2 runs): a failure may not be yours"},
		{"fp", "ab", false, ""}, // fixed by the next commit
		{"ffff", "abcd", false, "failed in the last recorded run"}, // broken, not flaky
		{"pfpf", "abcd", true, "flaky across runs: switched between passing and failing 3 times in the last 4 runs (failed in 2): a failure may not be yours"},
		{"p-sfp", "abcde", false, ""}, // skipped and absent runs neither count nor flip
		{"pp", "aa", false, ""},
	} {
		t.Run(tt.outcomes+"@"+tt.commits, func(t *testing.T) {
			f := runs(t, tt.outcomes, tt.commits)
			tr, ok := f.TrendOf(tf)
			require.True(t, ok)
			assert.Equal(t, tr.Flaky(), tt.flaky)
			note, _ := f.RunNote(tf)
			assert.Equal(t, note, tt.note)
		})
	}
}

func TestHistoryWindow(t *testing.T) {
	f := runs(t, strings.Repeat("p", MaxRuns+5), strings.Repeat("a", MaxRuns+5))
	assert.Equal(t, len(f.Runs), MaxRuns)
	assert.Equal(t, f.History[tf], strings.Repeat("p", MaxRuns))
	assert.Equal(t, f.Runs[0].At, time.Unix(5, 0), "the oldest runs are dropped")

	// A test that appears late is padded; one that stops being reported is
	// forgotten after forgetAfter runs.
	f = runs(t, "--p", "abc")
	assert.Equal(t, f.History[tf], "--p")
	for i := range forgetAfter {
		f = Facts{Commit: fmt.Sprint(i), Tests: map[string]TestRun{tg: {Passed: 1}}}.WithHistory(f)
		_, kept := f.History[tf]
		assert.Equal(t, kept, i < forgetAfter-1, fmt.Sprint("after ", i+1, " absent runs"))
	}
	for id, h := range f.History {
		assert.Equal(t, len(h), len(f.Runs), id)
	}
}
