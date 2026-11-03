package main

import (
	"context"
	"errors"
	"time"
)

// Feature health (docs/plans/FEATURE_HEALTH_PLAN.md): the hooks time their
// own runs and the pre-brief, keep them in the session's state file and send
// them with the next retrieval report. Only the feature's name, the duration
// and how it ended are kept; never the prompt, paths or what was answered.

// maxHookTimings bounds the runs a state file keeps between reports, and so
// one report (the server accepts at most as many); older runs are dropped.
const maxHookTimings = 64

// maxTimingMs is the longest run the server accepts.
const maxTimingMs = 10 * 60 * 1000

// The short names the server accepts (usage.ReportedFeatures).
const (
	timingPreBrief     = "pre-brief"
	timingSessionStart = "session-start"
	timingPromptRecall = "prompt-recall"
	timingStop         = "stop"
	timingReport       = "retrieval-report"
)

// hookTiming is one timed run.
type hookTiming struct {
	Feature    string `json:"feature"`
	DurationMs int    `json:"duration_ms"`
	Outcome    string `json:"outcome,omitempty"`
}

// timingOutcome is how a run that ended with err went: "" (ok), "timeout"
// when it ran out of time, else "error".
func timingOutcome(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return "error"
}

// noteTiming keeps one run of feature in the chat's state file, for the next
// retrieval report. A chat without a state file keeps nothing.
func noteTiming(session, feature string, took time.Duration, outcome string) {
	file := stateFile(session)
	if file == "" {
		return
	}
	s := loadState(file)
	s.addTiming(feature, took, outcome)
	saveState(file, s)
}

func (s *hookState) addTiming(feature string, took time.Duration, outcome string) {
	s.Timings = append(s.Timings, hookTiming{Feature: feature, DurationMs: int(min(max(took.Milliseconds(), 0), maxTimingMs)), Outcome: outcome})
	if len(s.Timings) > maxHookTimings {
		s.Timings = s.Timings[len(s.Timings)-maxHookTimings:]
	}
}
