package agentcfg

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// PluginHook is one handler the Claude Code plugin runs: its event, its tool
// matcher and the codesynapse arguments.
type PluginHook struct {
	Event   string `json:"event"`
	Matcher string `json:"matcher,omitempty"`
	Args    string `json:"args"`
}

func (h PluginHook) String() string {
	if h.Matcher == "" {
		return h.Event + ": codesynapse " + h.Args
	}
	return h.Event + " (" + h.Matcher + "): codesynapse " + h.Args
}

// ShippedHooks are the hooks the plugin of this release runs
// (plugins/codesynapse/hooks/hooks.json). TestPluginFiles keeps the two
// equal, so a CLI knows which hooks an installed plugin should have.
var ShippedHooks = []PluginHook{
	{Event: "SessionStart", Args: "hook session"},
	{Event: "SessionStart", Args: "memory recall --hook"},
	{Event: "PreCompact", Args: "memory offload --hook"},
	{Event: "PreCompact", Args: "hook lifecycle --client claude --event PreCompact"},
	{Event: "PreToolUse", Matcher: "Read", Args: "hook read"},
	{Event: "PreToolUse", Matcher: "Bash", Args: "hook bash"},
	{Event: "PreToolUse", Matcher: "WebFetch", Args: "hook webfetch"},
	{Event: "PreToolUse", Matcher: "Read|Grep|Glob|mcp__.*codesynapse.*", Args: "hook lookup"},
	{Event: "PostToolUse", Matcher: "mcp__.*codesynapse.*", Args: "hook answer"},
	{Event: "PostToolUse", Matcher: "Read", Args: "hook read-done"},
	{Event: "PostToolUse", Matcher: "Edit|Write", Args: "hook edit"},
	{Event: "PostToolUseFailure", Matcher: "Edit|Write", Args: "hook edit-failed"},
	{Event: "UserPromptSubmit", Args: "hook prompt"},
	{Event: "UserPromptSubmit", Args: "memory recall --hook"},
	{Event: "UserPromptSubmit", Args: "hook brief"},
	{Event: "CwdChanged", Args: "hook lifecycle --client claude --event CwdChanged"},
	{Event: "SessionEnd", Args: "hook lifecycle --client claude --event SessionEnd"},
	{Event: "SubagentStart", Args: "hook lifecycle --client claude --event SubagentStart"},
	{Event: "SubagentStop", Args: "hook lifecycle --client claude --event SubagentStop"},
	{Event: "PostCompact", Args: "hook lifecycle --client claude --event PostCompact"},
	{Event: "PostToolBatch", Args: "hook lifecycle --client claude --event PostToolBatch"},
	{Event: "InstructionsLoaded", Args: "hook lifecycle --client claude --event InstructionsLoaded"},
	{Event: "PermissionRequest", Args: "hook lifecycle --client claude --event PermissionRequest"},
	{Event: "PermissionDenied", Args: "hook lifecycle --client claude --event PermissionDenied"},
	{Event: "StopFailure", Args: "hook lifecycle --client claude --event StopFailure"},
	{Event: "Stop", Args: "hook stop"},
}

// maxHooksFile bounds a hooks.json read from an installed plugin.
const maxHooksFile = 256 << 10

// ParsePluginHooks reads the codesynapse handlers of a plugin's hooks.json,
// sorted and without duplicates. Handlers of other commands are ignored.
func ParsePluginHooks(data []byte) ([]PluginHook, error) {
	if len(data) > maxHooksFile {
		return nil, errors.New("hooks.json is too large")
	}
	var doc struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string   `json:"command"`
				Args    []string `json:"args"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("hooks.json: %w", err)
	}
	var out []PluginHook
	for event, groups := range doc.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				if h.Command == Name {
					out = append(out, PluginHook{Event: event, Matcher: g.Matcher, Args: strings.Join(h.Args, " ")})
				}
			}
		}
	}
	slices.SortFunc(out, comparePluginHooks)
	return slices.Compact(out), nil
}

func comparePluginHooks(a, b PluginHook) int {
	return cmp.Or(cmp.Compare(a.Event, b.Event), cmp.Compare(a.Matcher, b.Matcher), cmp.Compare(a.Args, b.Args))
}

// ReadPluginHooks reads root/hooks/hooks.json of an installed plugin.
func ReadPluginHooks(root string) ([]PluginHook, error) {
	f, err := os.Open(filepath.Join(root, "hooks", "hooks.json")) //nolint:gosec // the plugin's own folder, read only
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	data, err := io.ReadAll(io.LimitReader(f, maxHooksFile+1))
	if err != nil {
		return nil, err
	}
	return ParsePluginHooks(data)
}

// CompareHooks says which shipped hooks an installed plugin lacks (it is
// older than this CLI) and which it runs that this CLI does not ship (the
// CLI is older than the plugin).
func CompareHooks(installed []PluginHook) (missing, extra []PluginHook) {
	shipped := slices.Clone(ShippedHooks)
	slices.SortFunc(shipped, comparePluginHooks)
	has := func(list []PluginHook, h PluginHook) bool {
		_, ok := slices.BinarySearchFunc(list, h, comparePluginHooks)
		return ok
	}
	installed = slices.Clone(installed)
	slices.SortFunc(installed, comparePluginHooks)
	for _, h := range shipped {
		if !has(installed, h) {
			missing = append(missing, h)
		}
	}
	for _, h := range installed {
		if !has(shipped, h) {
			extra = append(extra, h)
		}
	}
	return missing, extra
}

// InstalledPlugin is the CodeSynapse plugin as Claude Code installed it.
type InstalledPlugin struct {
	Key         string // codesynapse@<marketplace>
	Marketplace string
	Scope       string // user, project, local or managed
	Path        string // the plugin's folder in Claude Code's cache
	Version     string
}

// safeName is a marketplace name that may go into a claude argument.
var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// maxInstalledFile bounds Claude Code's installed_plugins.json.
const maxInstalledFile = 1 << 20

// FindClaudePlugin looks the CodeSynapse plugin up in Claude Code's list of
// installed plugins. A user-scope install is preferred; a folder outside
// ~/.claude/plugins is ignored.
func FindClaudePlugin(home string) (InstalledPlugin, bool) {
	base := filepath.Join(home, ".claude", "plugins")
	f, err := os.Open(filepath.Join(base, "installed_plugins.json")) //nolint:gosec // Claude Code's own file
	if err != nil {
		return InstalledPlugin{}, false
	}
	defer f.Close() //nolint:errcheck // read-only
	data, err := io.ReadAll(io.LimitReader(f, maxInstalledFile+1))
	if err != nil || len(data) > maxInstalledFile {
		return InstalledPlugin{}, false
	}
	var doc struct {
		Plugins map[string][]struct {
			Scope       string `json:"scope"`
			InstallPath string `json:"installPath"`
			Version     string `json:"version"`
		} `json:"plugins"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return InstalledPlugin{}, false
	}
	var found []InstalledPlugin
	for key, entries := range doc.Plugins {
		market, ok := strings.CutPrefix(key, pluginPrefix)
		if !ok || !safeName.MatchString(market) {
			continue
		}
		for _, e := range entries {
			p := filepath.Clean(e.InstallPath)
			if !filepath.IsAbs(p) || !strings.HasPrefix(p, base+string(filepath.Separator)) {
				continue
			}
			found = append(found, InstalledPlugin{Key: key, Marketplace: market, Scope: e.Scope, Path: p, Version: e.Version})
		}
	}
	if len(found) == 0 {
		return InstalledPlugin{}, false
	}
	slices.SortFunc(found, func(a, b InstalledPlugin) int {
		return cmp.Or(cmp.Compare(scopeRank(a.Scope), scopeRank(b.Scope)), cmp.Compare(a.Key, b.Key), cmp.Compare(a.Path, b.Path))
	})
	return found[0], true
}

func scopeRank(s string) int {
	if s == "user" {
		return 0
	}
	return 1
}

// UpdateClaudePlugin has Claude Code refresh the plugin's marketplace and
// update the plugin, as /plugin does. Only user-scope installs are updated
// here: a project's plugins belong to that project.
func UpdateClaudePlugin(ctx context.Context, p InstalledPlugin) error {
	if p.Scope != "user" {
		return fmt.Errorf("the plugin is installed for a project (%s scope); update it there with /plugin", p.Scope)
	}
	if !safeName.MatchString(p.Marketplace) {
		return errors.New("unexpected marketplace name")
	}
	if err := run(ctx, "claude", "plugin", "marketplace", "update", p.Marketplace); err != nil {
		return err
	}
	return run(ctx, "claude", "plugin", "update", pluginPrefix+p.Marketplace, "--scope", "user")
}
