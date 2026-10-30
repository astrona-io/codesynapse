package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/agentrules"
	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/cliversion"
	"github.com/astrona-io/codesynapse/internal/conventions"
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
		Text          string `json:"text"`
		ServerVersion string `json:"server_version"`
	}
	if c.token != "" {
		if err := c.do(ctx, http.MethodGet, "/api/v1/agent-rules", nil, &res); err != nil {
			logger.Debug("agent rules not fetched; using the built-in text", "error", err)
		} else if t := strings.TrimSpace(res.Text); t != "" && len(t) <= agentrules.MaxBytes {
			text = t
		}
		text += repoConventions(ctx, c, in.Cwd, logger)
	}
	// Claude Code names the plugin's folder in the hook's environment.
	notice := updateNotice(version, res.ServerVersion, os.Getenv("CLAUDE_PLUGIN_ROOT"))
	var shown []string // systemMessage is shown to the person, not the model
	if notice != "" {
		text += "\n\n" + cliversion.ForAgent(notice)
		shown = append(shown, "CodeSynapse: "+notice)
	}
	out := map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "SessionStart", "additionalContext": text,
	}}
	if home, err := os.UserHomeDir(); err == nil {
		if warn := gatewayWarning(ctx, home); warn != "" {
			shown = append(shown, warn)
		}
	}
	if len(shown) > 0 {
		out["systemMessage"] = strings.Join(shown, "\n")
	}
	return out
}

// repoConventions is the house style of the checkout's repository, as a
// section to add to the session's rules, or "". Once per session costs less
// than in every minimal_context answer.
func repoConventions(ctx context.Context, c apiClient, cwd string, logger *slog.Logger) string {
	if cwd == "" {
		return ""
	}
	w := newWorktree(ctx, cwd, "", logger)
	if w == nil {
		return ""
	}
	var res struct {
		Conventions []string `json:"conventions"`
	}
	err := w.call(ctx, c, func(repo string) error {
		return c.do(ctx, http.MethodGet, "/api/v1/repos/"+url.PathEscape(repo)+"/conventions", nil, &res)
	})
	if err != nil {
		logger.Debug("conventions not fetched", "repo", w.repo, "error", err)
		return ""
	}
	var b strings.Builder
	for i, s := range res.Conventions {
		if i == conventions.MaxStatements {
			break
		}
		if s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " ")); s != "" && len(s) <= 300 {
			b.WriteString("\n- " + s)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n\nHouse style of " + w.repo + ", measured in its code by CodeSynapse; write new code the same way:" + b.String()
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
	s := loadState(state)
	if s.Base == nil { // a resumed session keeps where it began
		s.Base = baseOf(ctx, w.root)
	}
	if in.Source != "compact" {
		// The key Stop compares with, computed the same way (it once took the
		// bridge's tree fingerprint, which never matched, so a checkout dirty
		// at the start was asked about at the first stop). After compaction
		// the session goes on, and what it changed so far is still unchecked.
		s.Stop = stopChanges(ctx, w.root, s.Base).Key
	}
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
// began (once), the reads turned back and the WebFetch calls pointed at
// fetch_source. nil when there is nothing new.
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
	if d := s.Steers - s.Reported.Steers; d > 0 {
		out["fetches_steered"] = d
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *hookState) markReported() {
	s.Reported.First, s.Reported.Turned, s.Reported.Steers = s.First != "", s.Turned, s.Steers
}
