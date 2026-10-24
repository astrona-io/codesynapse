package runtimefacts

import (
	"fmt"
	"strings"
	"time"
)

// MaxRuns bounds the history: the last uploads whose outcomes are kept.
const MaxRuns = 20

// Run is one upload the history covers.
type Run struct {
	Commit string    `json:"commit,omitempty"`
	At     time.Time `json:"at"`
}

// Outcome letters in History, one per run.
const (
	outPassed  = 'p'
	outFailed  = 'f'
	outFlaky   = 'x' // passed and failed within the run
	outSkipped = 's'
	outAbsent  = '-' // not reported by that run
)

func outcome(t TestRun) byte {
	switch {
	case t.Flaky():
		return outFlaky
	case t.Failed > 0:
		return outFailed
	case t.Passed > 0:
		return outPassed
	case t.Skipped > 0:
		return outSkipped
	}
	return outAbsent
}

// forgetAfter is how many runs a test may go unreported before its history
// is dropped, so renamed and deleted tests do not pile up.
const forgetAfter = 5

// WithHistory carries prev's history forward into f, a new upload: f's run
// is appended to prev.Runs (the oldest dropped past MaxRuns), and every
// test gets its outcome in it.
func (f Facts) WithHistory(prev Facts) Facts {
	runs := append(append([]Run(nil), prev.Runs...), Run{Commit: f.Commit, At: f.MeasuredAt})
	drop := max(0, len(runs)-MaxRuns)
	runs = runs[drop:]
	n := len(prev.Runs)
	history := make(map[string]string, len(f.Tests))
	line := func(h string) string {
		// Histories written before a test existed are shorter: pad them.
		if len(h) < n {
			h = strings.Repeat(string(outAbsent), n-len(h)) + h
		}
		return h[len(h)-n:]
	}
	for id, h := range prev.History {
		if _, now := f.Tests[id]; !now {
			if h = line(h) + string(outAbsent); strings.Trim(h[max(0, len(h)-forgetAfter):], string(outAbsent)) != "" {
				history[id] = h[min(drop, len(h)):]
			}
		}
	}
	for id, t := range f.Tests {
		h := line(prev.History[id]) + string(outcome(t))
		history[id] = h[min(drop, len(h)):]
	}
	f.Runs, f.History = runs, history
	return f
}

// Trend is what the recorded runs say about one test.
type Trend struct {
	Runs, Failed int
	// SameCommit is a commit the test both passed and failed at (a CI rerun
	// that turned green, or two jobs), "" when there is none.
	SameCommit string
	// Flips counts changes between passing and failing from run to run.
	Flips int
}

// Flaky reports a test whose outcome changed without its commit changing,
// or that kept switching between passing and failing.
func (t Trend) Flaky() bool { return t.SameCommit != "" || t.Flips >= 3 }

// TrendOf reads test id's history.
func (f Facts) TrendOf(id string) (Trend, bool) {
	h, ok := f.History[id]
	if !ok || len(h) != len(f.Runs) {
		return Trend{}, false
	}
	var t Trend
	seen := map[string]byte{} // commit -> the outcomes seen at it, p|f bits
	var last byte
	for i := range len(h) {
		o := h[i]
		if o == outAbsent || o == outSkipped {
			continue
		}
		t.Runs++
		if o == outFailed || o == outFlaky {
			t.Failed++
		}
		bits := map[byte]byte{outPassed: 1, outFailed: 2, outFlaky: 3}[o]
		if c := f.Runs[i].Commit; c != "" {
			if seen[c] |= bits; seen[c] == 3 && t.SameCommit == "" {
				t.SameCommit = c
			}
		}
		if p := o == outPassed; last != 0 && p != (last == outPassed) {
			t.Flips++
		}
		last = map[bool]byte{true: outPassed, false: outFailed}[o == outPassed]
	}
	return t, t.Runs > 0
}

// trendNote says what the history adds to the last run, or "".
func (f Facts) trendNote(id string) string {
	t, ok := f.TrendOf(id)
	if !ok || t.Runs < 2 {
		return ""
	}
	switch {
	case t.SameCommit != "":
		return fmt.Sprintf("flaky across runs: passed and failed at commit %s (failed in %d of the last %d runs)", t.SameCommit[:min(len(t.SameCommit), 12)], t.Failed, t.Runs)
	case t.Flips >= 3:
		return fmt.Sprintf("flaky across runs: switched between passing and failing %d times in the last %d runs (failed in %d)", t.Flips, t.Runs, t.Failed)
	}
	return ""
}
