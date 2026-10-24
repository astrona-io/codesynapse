package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/agentcfg"
	"github.com/astrona-io/codesynapse/internal/agentrules"
	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/memory"
)

type lifecycleOptions struct {
	EC                    *app.Env
	client, event, apiURL string
}

func (o *lifecycleOptions) SetupFlags(context.Context, *app.Env) error {
	f := o.EC.Command.Flags()
	f.StringVar(&o.client, "client", "", "Assistant runtime: codex or cursor")
	f.StringVar(&o.event, "event", "", "Native lifecycle event name")
	f.StringVar(&o.apiURL, "api-url", app.Getenv("CODESYNAPSE_API_URL", "http://127.0.0.1:8080"), "API for runtime registration and activity")
	return nil
}
func (o *lifecycleOptions) Complete(context.Context, *app.Env) error { return nil }
func (o *lifecycleOptions) Validate(context.Context, *app.Env) error {
	if !slices.Contains(agentcfg.HookEvents(o.client), o.event) {
		return errors.New("unsupported assistant hook event")
	}
	return checkBridgeURL(o.apiURL)
}

// lifecycleInput is deliberately not a generic payload map. Raw content is
// measured in memory, never persisted, logged or sent to the API.
type lifecycleInput struct {
	ToolUseID            string          `json:"tool_use_id"`
	AgentID              string          `json:"agent_id"`
	TranscriptPath       string          `json:"transcript_path"`
	SessionID            string          `json:"session_id"`
	ConversationID       string          `json:"conversation_id"`
	ParentConversationID string          `json:"parent_conversation_id"`
	Event                string          `json:"hook_event_name"`
	Cwd                  string          `json:"cwd"`
	WorkspaceRoots       []string        `json:"workspace_roots"`
	ToolName             string          `json:"tool_name"`
	ServerName           string          `json:"mcp_server_name"`
	ToolInput            json.RawMessage `json:"tool_input"`
	ToolResponse         json.RawMessage `json:"tool_response"`
	ToolOutput           json.RawMessage `json:"tool_output"`
	Result               json.RawMessage `json:"result"`
	FilePath             string          `json:"file_path"`
	Prompt               string          `json:"prompt"`
	Text                 string          `json:"text"`
	Content              string          `json:"content"`
	Output               json.RawMessage `json:"output"`
	MessageCount         *int64          `json:"message_count"`
	ToolCallCount        *int64          `json:"tool_call_count"`
	MessagesToCompact    *int64          `json:"messages_to_compact"`
	ContextPercent       *float64        `json:"context_usage_percent"`
	FirstCompaction      *bool           `json:"is_first_compaction"`
	Background           *bool           `json:"is_background_agent"`
	Duration             *int64          `json:"duration"`
	ResultJSON           json.RawMessage `json:"result_json"`
	Command              string          `json:"command"`
	FailureType          string          `json:"failure_type"`
	Sandbox              *bool           `json:"sandbox"`
	PermissionMode       string          `json:"permission_mode"`
	IsInterrupt          bool            `json:"is_interrupt"`
	Source               string          `json:"source"`
	Trigger              string          `json:"trigger"`
	Status               string          `json:"status"`
	DurationMS           *int64          `json:"duration_ms"`
	ContextTokens        *int64          `json:"context_tokens"`
	ContextLimit         *int64          `json:"context_window_size"`
	LoopCount            int64           `json:"loop_count"`
	StopHookActive       bool            `json:"stop_hook_active"`
}

func decodeLifecycle(in io.Reader, client, event string) (lifecycleInput, toolHookInput, agentcfg.HookActivity, error) {
	var raw lifecycleInput
	b, err := io.ReadAll(io.LimitReader(in, (8<<20)+1))
	if err != nil || len(b) > 8<<20 {
		return raw, toolHookInput{}, agentcfg.HookActivity{}, errors.New("hook input exceeds limit")
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return raw, toolHookInput{}, agentcfg.HookActivity{}, errors.New("invalid hook JSON")
	}
	if raw.Event != "" && raw.Event != event {
		return raw, toolHookInput{}, agentcfg.HookActivity{}, errors.New("hook event mismatch")
	}
	if client == "cursor" {
		if raw.ConversationID != "" {
			if raw.SessionID != "" && raw.SessionID != raw.ConversationID {
				return raw, toolHookInput{}, agentcfg.HookActivity{}, execution.ErrConflict
			}
			raw.SessionID = raw.ConversationID
		}
		if raw.ParentConversationID != "" && raw.ParentConversationID != raw.SessionID {
			return raw, toolHookInput{}, agentcfg.HookActivity{}, execution.ErrConflict
		}
	}
	if err := execution.Validate(execution.Identity{TenantID: "t", SubjectID: "u", SessionID: raw.SessionID, ParentSessionID: raw.SessionID}); err != nil {
		return raw, toolHookInput{}, agentcfg.HookActivity{}, execution.ErrMissing
	}
	if raw.Cwd == "" && len(raw.WorkspaceRoots) == 1 {
		raw.Cwd = raw.WorkspaceRoots[0]
	}
	tool := toolHookInput{SessionID: raw.SessionID, Cwd: raw.Cwd, ToolName: raw.ToolName, StopHookActive: raw.StopHookActive || raw.LoopCount > 0, Client: client, TranscriptPath: raw.TranscriptPath}
	input := raw.ToolInput
	if len(input) > 0 && input[0] == '"' {
		var s string
		if json.Unmarshal(input, &s) == nil {
			if raw.ToolName == "apply_patch" {
				tool.ToolInput.Patch = s
				input = nil
			} else {
				input = []byte(s)
			}
		}
	}
	if len(input) > 0 && !bytes.Equal(input, []byte("null")) {
		if err := json.Unmarshal(input, &tool.ToolInput); err != nil {
			return raw, tool, agentcfg.HookActivity{}, errors.New("invalid tool input")
		}
		if tool.ToolInput.FilePath == "" {
			var aliases struct {
				Path string `json:"path"`
			}
			if json.Unmarshal(input, &aliases) == nil {
				tool.ToolInput.FilePath = aliases.Path
			}
		}
	}
	if raw.FilePath != "" {
		tool.ToolInput.FilePath = raw.FilePath
	}
	if raw.ServerName == agentcfg.Name {
		tool.ToolName = "mcp__codesynapse__" + raw.ToolName
	}
	tool.ToolResponse = raw.ToolResponse
	if len(tool.ToolResponse) == 0 {
		tool.ToolResponse = raw.ToolOutput
	}
	if len(tool.ToolResponse) == 0 {
		tool.ToolResponse = raw.Result
	}
	if len(tool.ToolResponse) == 0 {
		tool.ToolResponse = raw.Output
	}
	if len(tool.ToolResponse) == 0 {
		tool.ToolResponse = raw.ResultJSON
	}
	category := hookCategory(tool.ToolName)
	if event == "beforeReadFile" || event == "beforeTabFileRead" {
		category = "read"
		tool.ToolName = "Read"
	}
	if strings.Contains(strings.ToLower(event), "mcp") {
		category = "mcp"
	}
	if strings.Contains(strings.ToLower(event), "shell") {
		category = "shell"
	}
	if strings.Contains(strings.ToLower(event), "subagent") {
		category = "subagent"
	}
	if event == "afterFileEdit" || event == "afterTabFileEdit" {
		category = "write"
	}
	a := agentcfg.HookActivity{Client: client, Event: event, HookMeasurements: agentcfg.HookMeasurements{
		Category: category, Outcome: "unknown", DurationMS: raw.DurationMS,
		InputBytes: int64(len(raw.ToolInput)), OutputBytes: int64(len(tool.ToolResponse)),
		PromptBytes: int64(len(raw.Prompt)), TextBytes: int64(len(raw.Text)),
		ContextTokens: raw.ContextTokens, ContextLimit: raw.ContextLimit, LoopCount: raw.LoopCount,
	}}
	a.Tool = toolOf(tool.ToolName)
	if a.DurationMS == nil {
		a.DurationMS = raw.Duration
	}
	if strings.Contains(strings.ToLower(event), "shell") && len(raw.ToolInput) == 0 {
		a.InputBytes = int64(len(raw.Command))
	}
	if slices.Contains([]string{"timeout", "error", "permission_denied"}, raw.FailureType) {
		a.FailureType = raw.FailureType
	}
	if slices.Contains([]string{"default", "acceptEdits", "plan", "dontAsk", "bypassPermissions"}, raw.PermissionMode) {
		a.PermissionMode = raw.PermissionMode
	}
	a.Sandbox = raw.Sandbox
	a.ContentBytes = int64(len(raw.Content))
	a.MessageCount, a.ToolCallCount, a.MessagesToCompact = raw.MessageCount, raw.ToolCallCount, raw.MessagesToCompact
	a.ContextPercent, a.FirstCompaction, a.Background = raw.ContextPercent, raw.FirstCompaction, raw.Background
	if slices.Contains([]string{"startup", "resume", "clear", "compact", "fork", "manual", "auto", "other"}, raw.Source) {
		a.Source = raw.Source
	}
	if a.Source == "" && slices.Contains([]string{"manual", "auto"}, raw.Trigger) {
		a.Source = raw.Trigger
	}
	switch raw.Status {
	case "completed", "success":
		a.Outcome = "ok"
	case "error", "failed":
		a.Outcome = "error"
	case "aborted":
		a.Outcome = "aborted"
	}
	if event == "postToolUseFailure" {
		a.Outcome = "error"
	}
	if event == "postToolUse" {
		a.Outcome = "ok"
	}
	if raw.IsInterrupt {
		a.Outcome = "aborted"
	}
	if event == "Interrupt" {
		a.Outcome = "aborted"
	}
	return raw, tool, a, a.Validate()
}

func hookCategory(name string) string {
	switch name {
	case "":
		return "none"
	case "Read", "read_file":
		return "read"
	case "Edit", "Write", "apply_patch":
		return "write"
	case "Grep", "Glob", "Search":
		return "search"
	case "Bash", "Shell", "exec_command":
		return "shell"
	case "Task", "Agent", "spawn_agent":
		return "subagent"
	}
	if strings.HasPrefix(name, "mcp__") {
		return "mcp"
	}
	return "other"
}

func (o *lifecycleOptions) Run(ctx context.Context, _ *app.Env) error {
	// Advisory hooks never break the assistant on a network or parse failure.
	// Native permission decisions remain with the assistant and its user.
	if err := o.Validate(ctx, nil); err != nil {
		return err
	}
	raw, tool, activity, err := decodeLifecycle(o.EC.Stdin, o.client, o.event)
	if err != nil {
		if o.event != "workspaceOpen" && o.event != "beforeTabFileRead" && o.event != "afterTabFileEdit" {
			o.EC.Logger.Warn("assistant hook skipped: missing or invalid runtime input")
		}
		return json.NewEncoder(o.EC.Stdout).Encode(lifecycleDefault(o.client, o.event))
	}
	budget := 10 * time.Second
	if o.event == "SessionEnd" || o.event == "Interrupt" {
		budget = 2500 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	c := newAPIClient(o.apiURL)
	primary := os.Getenv(cursorCLISessionEnv)
	if o.client == "cursor" && primary == "" {
		var lookupErr error
		primary, lookupErr = findCursorLaunch(ctx, cursorLaunchDir(), c.token)
		if lookupErr != nil {
			return lookupErr
		}
	}
	if o.client == "cursor" && primary != "" && primary != tool.SessionID {
		if err := writeRuntimeReceipt(c, "cursor", "thread", "cli-mismatch:"+primary, tool.SessionID); err != nil {
			o.EC.Logger.Warn("Cursor chat switch could not be recorded; restart the CLI")
		}
		return json.NewEncoder(o.EC.Stdout).Encode(map[string]any{"permission": "deny", "user_message": "Restart with codesynapse cursor for this chat."})
	}

	if o.client == "cursor" && (o.event == "beforeMCPExecution" || o.event == "preToolUse" && (activity.Category == "mcp" || raw.ToolName == "MCP" || strings.HasPrefix(raw.ToolName, "MCP:"))) {
		if receiptErr := writeRuntimeReceipt(c, "cursor", "call", raw.ToolUseID, tool.SessionID); receiptErr != nil {
			o.EC.Logger.Warn("Cursor call identity receipt unavailable; MCP invocation will be refused")
		}
	}
	if o.client == "codex" && o.event == "SubagentStart" {
		if receiptErr := writeRuntimeReceipt(c, "codex", "thread", raw.AgentID, tool.SessionID); receiptErr != nil {
			o.EC.Logger.Warn("Codex subagent identity receipt unavailable; delegated MCP invocation will be refused")
		}
	}
	registered, registerErr := c.executionContext(ctx, tool.SessionID, tool.SessionID)
	err = registerErr
	if err == nil {
		ctx = registered
	}
	if err == nil {
		err = c.do(ctx, http.MethodPost, "/api/v1/execution/activity", activity, nil)
	}
	if err != nil {
		o.EC.Logger.Warn("assistant activity not saved; check API key, endpoint and runtime registration")
	}
	out := map[string]any{}
	switch o.event {
	case "SessionStart", "sessionStart", "SubagentStart", "UserPromptSubmit", "PostCompact":
		if o.event == "SessionStart" || o.event == "sessionStart" {
			if _, statErr := os.Stat(stateFile(tool.SessionID)); os.IsNotExist(statErr) {
				noteBaseline(ctx, tool, o.EC.Logger)
			}
		}
		text := agentrules.Default
		if err == nil {
			var rules struct {
				Text string `json:"text"`
			}
			if c.do(ctx, http.MethodGet, "/api/v1/agent-rules", nil, &rules) == nil && len(rules.Text) > 0 && len(rules.Text) <= agentrules.MaxBytes {
				text = rules.Text
			}
			if o.event == "SessionStart" || o.event == "sessionStart" {
				if repo, resolveErr := c.resolveRepo(ctx, tool.Cwd); resolveErr == nil {
					var facts memory.Graph
					if c.do(ctx, http.MethodGet, "/api/v1/repos/"+url.PathEscape(repo)+"/session-memory?limit=20", nil, &facts) == nil {
						text += "\n" + formatFacts(repo, facts, 6000)
					}
				}
			}
		}
		if o.client == "cursor" {
			out["additional_context"] = text
		} else {
			out["hookSpecificOutput"] = map[string]any{"hookEventName": o.event, "additionalContext": text}
		}
	case "PreToolUse", "preToolUse", "beforeReadFile", "beforeMCPExecution":
		noteLookup(tool)
		if activity.Category == "read" {
			out = readGuard(tool)
		}
		if o.client == "cursor" && out != nil {
			spec, _ := out["hookSpecificOutput"].(map[string]any)
			out = map[string]any{"permission": "deny", "user_message": spec["permissionDecisionReason"]}
			if o.event == "preToolUse" {
				out["agent_message"] = spec["permissionDecisionReason"]
			}
		}
	case "PostToolUse", "postToolUse", "afterMCPExecution":
		noteEdit(tool, false)
		noteAnswer(tool)
		if activity.Category == "read" {
			noteRead(tool)
		}
	case "PostToolUseFailure", "postToolUseFailure":
		noteEdit(tool, true)
	case "Stop", "stop":
		reportRetrieval(ctx, tool, o.EC.Logger)
		if raw.Status != "error" && raw.Status != "aborted" {
			out = stopGuard(ctx, tool, o.EC.Logger)
		}
		if o.client == "cursor" && out != nil {
			out = map[string]any{"followup_message": out["reason"]}
		}
	case "PreCompact", "preCompact", "SubagentStop", "subagentStop", "SessionEnd", "sessionEnd", "Interrupt":
		reportRetrieval(ctx, tool, o.EC.Logger)
	}
	if len(out) == 0 {
		out = lifecycleDefault(o.client, o.event)
	}
	return json.NewEncoder(o.EC.Stdout).Encode(out)
}

func lifecycleDefault(client, event string) map[string]any {
	if client == "cursor" {
		switch event {
		case "preToolUse", "beforeShellExecution", "beforeMCPExecution", "beforeReadFile", "beforeTabFileRead", "subagentStart":
			return map[string]any{"permission": "allow"}
		case "beforeSubmitPrompt":
			return map[string]any{"continue": true}
		}
	}
	return map[string]any{}
}
