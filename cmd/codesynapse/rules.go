package main

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/agentrules"
	"github.com/astrona-io/codesynapse/internal/app"
)

// sessionRules is the SessionStart hook: the ruleset a session starts with.
// It asks the server so the wording can be improved for every machine at
// once, and falls back to the built-in text when the server is unreachable
// or answers nothing usable. It never fails the session.
func sessionRules(ctx context.Context, in toolHookInput, logger *slog.Logger) map[string]any {
	noteBaseline(ctx, in, logger)
	text := agentrules.Default
	c := newAPIClient(app.Getenv("CODESYNAPSE_API_URL", "http://127.0.0.1:8080"))
	c.http.Timeout = 3 * time.Second
	var res struct {
		Text string `json:"text"`
	}
	if c.token != "" {
		if err := c.do(ctx, http.MethodGet, "/api/v1/agent-rules", nil, &res); err != nil {
			logger.Debug("agent rules not fetched; using the built-in text", "error", err)
		} else if t := strings.TrimSpace(res.Text); t != "" && len(t) <= agentrules.MaxBytes {
			text = t
		}
	}
	return map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "SessionStart", "additionalContext": text,
	}}
}

// noteBaseline remembers the working tree a session starts with, so the Stop
// hook asks for a check only when the session itself changed something: a
// checkout that was already dirty (an untracked scratch file, last week's
// edits) is not this session's change.
func noteBaseline(ctx context.Context, in toolHookInput, logger *slog.Logger) {
	state := stateFile(in.SessionID)
	if state == "" || in.Cwd == "" {
		return
	}
	w := newWorktree(ctx, in.Cwd, "", logger)
	if w == nil {
		return
	}
	key, _, err := w.changes(ctx)
	if err != nil {
		return
	}
	s := loadState(state)
	s.Stop = key // "" for a clean tree: any later change differs from it
	saveState(state, s)
}

// noteLookup records how the session began: the first of Read, Grep, Glob
// and the CodeSynapse tools decides. Later lookups change nothing.
func noteLookup(in toolHookInput) {
	state := stateFile(in.SessionID)
	if state == "" {
		return
	}
	kind := ""
	switch {
	case toolOf(in.ToolName) != "":
		kind = "codesynapse"
	case in.ToolName == "Read" || in.ToolName == "Grep" || in.ToolName == "Glob":
		kind = "native"
	}
	if kind == "" {
		return
	}
	s := loadState(state)
	if s.First != "" {
		return
	}
	s.First = kind
	saveState(state, s)
}

// workflowReport is what happened since the last report: how the session
// began (once) and the reads turned back. nil when there is nothing new.
func (s hookState) workflowReport() map[string]int {
	out := map[string]int{}
	if !s.Reported.First && s.First != "" {
		if s.First == "codesynapse" {
			out["first_codesynapse"] = 1
		} else {
			out["first_native"] = 1
		}
	}
	if d := s.Turned - s.Reported.Turned; d > 0 {
		out["reads_turned_back"] = d
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *hookState) markReported() {
	s.Reported = workflowSent{First: s.First != "", Turned: s.Turned}
}
