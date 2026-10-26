package agentcfg

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// pluginPrefix starts the enabledPlugins key of the CodeSynapse plugin
// ("codesynapse@<marketplace>").
const pluginPrefix = Name + "@"

// ClaudePluginEnabled reports whether the CodeSynapse Claude Code plugin is
// enabled in the user's settings or in the project's (dir). The plugin
// brings the bridge and every hook, so registering them again would run each
// tool and hook twice.
func ClaudePluginEnabled(home, dir string) bool {
	files := []string{filepath.Join(home, ".claude", "settings.json")}
	if dir != "" {
		files = append(files, filepath.Join(dir, ".claude", "settings.json"), filepath.Join(dir, ".claude", "settings.local.json"))
	}
	for _, f := range files {
		var doc struct {
			EnabledPlugins map[string]any `json:"enabledPlugins"`
		}
		data, err := os.ReadFile(f) //nolint:gosec // fixed settings paths under home or the project
		if err != nil || json.Unmarshal(data, &doc) != nil {
			continue
		}
		for k, v := range doc.EnabledPlugins {
			if on, _ := v.(bool); on && strings.HasPrefix(k, pluginPrefix) {
				return true
			}
		}
	}
	return false
}

// claudeHookCommand matches the hook commands `mcp hooks` and the lifecycle
// installer wrote into Claude Code settings before the plugin ran them:
// "'/path/codesynapse' memory recall --hook …" and "… hook lifecycle --client claude …".
var claudeHookCommand = regexp.MustCompile(`codesynapse'? (memory (recall|offload) |hook lifecycle --client claude )`)

// hasClaudeHooks reports whether a settings file holds CodeSynapse hooks.
func hasClaudeHooks(file string) bool {
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	data, err := os.ReadFile(file) //nolint:gosec // fixed settings path in the project
	if err != nil || json.Unmarshal(data, &doc) != nil {
		return false
	}
	for _, groups := range doc.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				if claudeHookCommand.MatchString(h.Command) {
					return true
				}
			}
		}
	}
	return false
}

// RemoveClaudeHooks deletes CodeSynapse handlers from a Claude Code settings
// file, keeping every other hook and setting. It returns how many it removed;
// a missing file removes none.
func RemoveClaudeHooks(file string) (int, error) {
	data, err := os.ReadFile(file) //nolint:gosec // fixed settings paths under home or the project
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	doc := map[string]any{}
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &doc); err != nil {
			return 0, fmt.Errorf("%s is not plain JSON (comments?); remove the CodeSynapse hooks by hand: %w", file, err)
		}
	}
	hooks, _ := doc["hooks"].(map[string]any)
	removed := 0
	for event, v := range hooks {
		groups, _ := v.([]any)
		var kept []any
		for _, g := range groups {
			m, ok := g.(map[string]any)
			handlers, hok := m["hooks"].([]any)
			if !ok || !hok {
				kept = append(kept, g)
				continue
			}
			left := slices.DeleteFunc(slices.Clone(handlers), func(h any) bool {
				cmd, _ := h.(map[string]any)["command"].(string)
				return claudeHookCommand.MatchString(cmd)
			})
			removed += len(handlers) - len(left)
			if len(left) > 0 {
				m["hooks"] = left
				kept = append(kept, m)
			}
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	if removed == 0 {
		return 0, nil
	}
	if len(hooks) == 0 {
		delete(doc, "hooks")
	}
	return removed, mergeJSON(file, nil, doc)
}

// claudeUserServer reports whether ~/.claude.json registers a user-scope
// codesynapse server, which `mcp install` added before the plugin existed.
func claudeUserServer(home string) bool {
	var doc struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude.json")) //nolint:gosec // Claude Code's own config file
	if err != nil || json.Unmarshal(data, &doc) != nil {
		return false
	}
	_, ok := doc.MCPServers[Name]
	return ok
}
