package agentcfg

import (
	"encoding/json"
	"errors"
	"math"
	"slices"

	"github.com/astrona-io/codesynapse/internal/toolsets"
)

// HookActivity is an allowlist of observations, never the original hook payload.
// Pointers distinguish an unreported measurement from a reported zero.
type HookActivity struct {
	Client string `json:"client"`
	Event  string `json:"event"`
	HookMeasurements
}

// HookMeasurements fits in the audit detail field. Only fixed enums and counts
// belong here: no prompt, command, path, model-generated text or raw tool name.
type HookMeasurements struct {
	Category          string   `json:"category"`
	Outcome           string   `json:"outcome"`
	Source            string   `json:"source,omitempty"`
	Tool              string   `json:"codesynapse_tool,omitempty"`
	DurationMS        *int64   `json:"duration_ms,omitempty"`
	InputBytes        int64    `json:"input_bytes,omitempty"`
	OutputBytes       int64    `json:"output_bytes,omitempty"`
	PromptBytes       int64    `json:"prompt_bytes,omitempty"`
	TextBytes         int64    `json:"text_bytes,omitempty"`
	ContextTokens     *int64   `json:"context_tokens,omitempty"`
	ContextLimit      *int64   `json:"context_limit,omitempty"`
	InputTokens       *int64   `json:"input_tokens,omitempty"`
	OutputTokens      *int64   `json:"output_tokens,omitempty"`
	CacheTokens       *int64   `json:"cache_tokens,omitempty"`
	LoopCount         int64    `json:"loop_count,omitempty"`
	ContentBytes      int64    `json:"content_bytes,omitempty"`
	MessageCount      *int64   `json:"message_count,omitempty"`
	ToolCallCount     *int64   `json:"tool_call_count,omitempty"`
	MessagesToCompact *int64   `json:"messages_to_compact,omitempty"`
	ContextPercent    *float64 `json:"context_usage_percent,omitempty"`
	FirstCompaction   *bool    `json:"is_first_compaction,omitempty"`
	Background        *bool    `json:"is_background_agent,omitempty"`
	FailureType       string   `json:"failure_type,omitempty"`
	Sandbox           *bool    `json:"sandbox,omitempty"`
	PermissionMode    string   `json:"permission_mode,omitempty"`
}

// Validate refuses rather than truncates metadata, keeping stored JSON valid.
func (a HookActivity) Validate() error {
	bad := errors.New("invalid agent hook activity")
	if !slices.Contains([]string{"", "timeout", "error", "permission_denied"}, a.FailureType) || !slices.Contains([]string{"", "default", "acceptEdits", "plan", "dontAsk", "bypassPermissions"}, a.PermissionMode) {
		return bad
	}
	if a.Tool != "" && !toolsets.Known(a.Tool) {
		return bad
	}
	if a.ContextPercent != nil && (math.IsNaN(*a.ContextPercent) || math.IsInf(*a.ContextPercent, 0) || *a.ContextPercent < 0 || *a.ContextPercent > 100) {
		return bad
	}
	if !slices.Contains(HookEvents(a.Client), a.Event) ||
		!slices.Contains([]string{"none", "read", "write", "search", "shell", "mcp", "subagent", "other"}, a.Category) ||
		!slices.Contains([]string{"unknown", "ok", "error", "aborted"}, a.Outcome) ||
		!slices.Contains([]string{"", "startup", "resume", "clear", "compact", "fork", "manual", "auto", "other"}, a.Source) {
		return bad
	}
	for _, n := range []int64{a.InputBytes, a.OutputBytes, a.PromptBytes, a.TextBytes, a.ContentBytes, a.LoopCount} {
		if n < 0 || n > 1<<30 {
			return bad
		}
	}
	for _, n := range []*int64{a.DurationMS, a.ContextTokens, a.ContextLimit, a.InputTokens, a.OutputTokens, a.CacheTokens, a.MessageCount, a.ToolCallCount, a.MessagesToCompact} {
		if n != nil && (*n < 0 || *n > 1<<30) {
			return bad
		}
	}
	b, err := json.Marshal(a.HookMeasurements)
	if err != nil || len(b) > 500 {
		return bad
	}
	return nil
}
