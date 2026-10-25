package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/execution"
)

// claudeEvents maps the plugin's own hook commands to the Claude Code event
// they run for. Each event is mapped from one command only (PreToolUse on
// Read goes through hook read and hook lookup, so only lookup reports), or an
// event would be counted twice.
var claudeEvents = map[string]string{
	"session":   "SessionStart",
	"prompt":    "UserPromptSubmit",
	"lookup":    "PreToolUse",
	"bash":      "PreToolUse",
	"answer":    "PostToolUse",
	"read-done": "PostToolUse",
	"edit":      "PostToolUse",
	// edit-failed runs on PostToolUseFailure.
	"edit-failed": "PostToolUseFailure",
	"stop":        "Stop",
}

// activityBudget keeps a slow API from slowing tool calls down.
const activityBudget = 1500 * time.Millisecond

// recordActivity saves the metadata-only activity of a plugin hook run, the
// same observation the lifecycle adapter makes for the events it handles, so
// every Claude Code hook reaches CodeSynapse. It never fails the hook:
// without an API key, a session ID or a reachable API it does nothing.
// CODESYNAPSE_HOOK_ACTIVITY=false turns it off.
func recordActivity(ctx context.Context, command string, raw []byte, logger *slog.Logger) string {
	event, ok := claudeEvents[command]
	if !ok {
		return "not reported: no activity event for this hook"
	}
	if os.Getenv("CODESYNAPSE_HOOK_ACTIVITY") == "false" {
		return "not reported: CODESYNAPSE_HOOK_ACTIVITY=false"
	}
	_, tool, a, err := decodeLifecycle(bytes.NewReader(raw), "claude", event)
	if err != nil {
		return "not reported: hook input not understood"
	}
	state := stateFile(tool.SessionID)
	c := newAPIClient(app.Getenv("CODESYNAPSE_API_URL", "http://127.0.0.1:8080"))
	if state == "" {
		return "not reported: no valid session ID"
	}
	if c.token == "" {
		return "not reported: no API token (CODESYNAPSE_API_TOKEN or the user config api-token file)"
	}
	ctx, cancel := context.WithTimeout(ctx, activityBudget)
	defer cancel()
	s := loadState(state)
	if s.Registered {
		// The headers carry only the session ID: the server resolves the
		// tenant and user from the key and the registration, so a session
		// registered by an earlier hook needs no second round trip.
		ctx, err = execution.With(ctx, execution.Identity{TenantID: "local", SubjectID: "local", SessionID: tool.SessionID, ParentSessionID: tool.SessionID})
	} else {
		ctx, err = c.executionContext(ctx, tool.SessionID, tool.SessionID)
		if err == nil {
			s.Registered = true
			saveState(state, s)
		}
	}
	if err == nil {
		err = c.do(ctx, http.MethodPost, "/api/v1/execution/activity", a, nil)
	}
	if err != nil {
		logger.Debug("hook activity not saved", "event", event, "error", err)
		return "not reported: " + err.Error()
	}
	return "reported to " + c.base
}
