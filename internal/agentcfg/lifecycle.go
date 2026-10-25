package agentcfg

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
)

// HookEvents is the documented event surface, including advisory events for
// which CodeSynapse only records activity and never makes a permission decision.
func HookEvents(client string) []string {
	switch client {
	case "claude":
		// Every event the plugin handles. hooks.json runs the lifecycle
		// adapter for those its own hook commands do not cover; the commands
		// report the others themselves (cmd/codesynapse/activity.go). Most only
		// record activity; SessionEnd and SubagentStop flush counts and
		// SubagentStart supplies the rules.
		return []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "PostToolUseFailure", "Stop", "PreCompact", "SessionEnd", "SubagentStart", "SubagentStop", "PostCompact", "PostToolBatch", "InstructionsLoaded", "CwdChanged", "PermissionRequest", "PermissionDenied", "StopFailure"}
	case "codex":
		return []string{"SessionStart", "SessionEnd", "UserPromptSubmit", "PreToolUse", "PermissionRequest", "PostToolUse", "PreCompact", "PostCompact", "SubagentStart", "SubagentStop", "Stop", "Interrupt"}
	case "cursor":
		return []string{"sessionStart", "sessionEnd", "beforeSubmitPrompt", "preToolUse", "postToolUse", "postToolUseFailure", "subagentStart", "subagentStop", "beforeShellExecution", "afterShellExecution", "beforeMCPExecution", "afterMCPExecution", "beforeReadFile", "afterFileEdit", "preCompact", "stop", "afterAgentResponse", "afterAgentThought", "beforeTabFileRead", "afterTabFileEdit", "workspaceOpen"}
	}
	return nil
}

// ShellQuote keeps paths and endpoints literal in the command string that the
// assistant's hook runner executes. Credentials never go in this string.
func ShellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// InstallLifecycleHooks preserves other hooks and settings, backs up the old
// document, and replaces only handlers with our stable adapter command marker.
func InstallLifecycleHooks(file, client, binary, apiURL string) error {
	events := HookEvents(client)
	if len(events) == 0 || binary == "" {
		return fmt.Errorf("unsupported hook client or missing binary: %q", client)
	}
	root := map[string]any{}
	if b, err := os.ReadFile(file); err == nil { //nolint:gosec // installer-selected settings path
		if err := json.Unmarshal(b, &root); err != nil || root == nil {
			return fmt.Errorf("%s must be a JSON object", file)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	hooks := map[string]any{}
	if v, exists := root["hooks"]; exists {
		var ok bool
		hooks, ok = v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: hooks must be an object", file)
		}
	}
	for _, event := range events {
		var kept []any
		if v, exists := hooks[event]; exists {
			groups, ok := v.([]any)
			if !ok {
				return fmt.Errorf("%s: %s must be an array", file, event)
			}
			for _, v := range groups {
				g, ok := v.(map[string]any)
				if !ok {
					return fmt.Errorf("%s: malformed %s hook", file, event)
				}
				if client == "cursor" {
					cmd, _ := g["command"].(string)
					if !strings.Contains(cmd, " hook lifecycle --client "+client+" ") {
						kept = append(kept, g)
					}
					continue
				}
				// A matcher group can contain unrelated handlers; retain them.
				handlers, ok := g["hooks"].([]any)
				if !ok {
					return fmt.Errorf("%s: malformed %s handlers", file, event)
				}
				remaining := slices.DeleteFunc(slices.Clone(handlers), func(v any) bool {
					h, _ := v.(map[string]any)
					cmd, _ := h["command"].(string)
					return strings.Contains(cmd, " hook lifecycle --client "+client+" ")
				})
				if len(remaining) > 0 {
					g["hooks"] = remaining
					kept = append(kept, g)
				}
			}
		}
		command := ShellQuote(binary) + " hook lifecycle --client " + client + " --event " + event + " --api-url " + ShellQuote(apiURL)
		handler := map[string]any{"command": command}
		if client == "codex" {
			handler["type"] = "command"
			handler["timeout"] = 15
			if event == "SessionEnd" || event == "Interrupt" {
				handler["timeout"] = 3
			}
			kept = append(kept, map[string]any{"hooks": []any{handler}})
		} else {
			kept = append(kept, handler)
		}
		hooks[event] = kept
	}
	if client == "cursor" {
		if v, exists := root["version"]; exists && v != float64(1) {
			return fmt.Errorf("%s: unsupported Cursor hooks version", file)
		}
		root["version"] = 1
	}
	root["hooks"] = hooks
	return mergeJSON(file, nil, root)
}
