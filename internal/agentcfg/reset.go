package agentcfg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Action is one thing `codesynapse doctor --reset` takes out again.
type Action struct {
	Description string
	apply       func(ctx context.Context) error
}

// Apply removes what the action describes.
func (a Action) Apply(ctx context.Context) error { return a.apply(ctx) }

// ResetOptions say where to look. Home is the user's home directory, Dir the
// project directory, TokenFile the private key file the bridge and hooks read.
type ResetOptions struct {
	Home, Dir, TokenFile string
	// KeepToken leaves TokenFile in place.
	KeepToken bool
}

// PlanReset finds what `mcp install`, `mcp hooks` and `mcp agents-md` left
// behind and returns one action per leftover, so nothing is listed that is
// not there. Notes are things it will not touch: the Claude Code plugin, the
// project's shared settings and caps that may be the user's own.
func PlanReset(o ResetOptions) (actions []Action, notes []string) {
	add := func(desc string, apply func(context.Context) error) {
		actions = append(actions, Action{Description: desc, apply: apply})
	}
	onPath := func(bin string) bool { _, err := exec.LookPath(bin); return err == nil }

	claudeSettings := filepath.Join(o.Home, ".claude", "settings.json")
	switch {
	case !claudeUserServer(o.Home):
	case onPath("claude"):
		add("Claude Code: user-scope "+Name+" MCP server", func(ctx context.Context) error {
			return run(ctx, "claude", "mcp", "remove", Name, "--scope", "user")
		})
	default:
		notes = append(notes, "~/.claude.json registers a "+Name+" server, but the claude CLI is not on PATH: run `claude mcp remove "+Name+" --scope user`")
	}
	for _, f := range []string{claudeSettings, filepath.Join(o.Dir, ".claude", "settings.local.json")} {
		if hasClaudeHooks(f) {
			add("Claude Code hooks in "+f, func(context.Context) error { _, err := RemoveClaudeHooks(f); return err })
		}
	}
	if hasGateway(claudeSettings) {
		add("Claude Code gateway settings in "+claudeSettings, func(context.Context) error {
			_, err := RemoveClaudeGateway(claudeSettings, time.Now())
			return err
		})
	}
	if shared := filepath.Join(o.Dir, ".claude", "settings.json"); hasClaudeHooks(shared) {
		notes = append(notes, shared+" is shared with the repository and has CodeSynapse hooks: remove them there by hand")
	}
	if ClaudePluginEnabled(o.Home, o.Dir) {
		notes = append(notes, "the CodeSynapse Claude Code plugin is enabled and brings its own bridge and hooks: disable it with /plugin")
	}

	codexConfig := filepath.Join(o.Home, ".codex", "config.toml")
	if fileContains(codexConfig, "[mcp_servers."+Name+"]") {
		if onPath("codex") {
			add("Codex: "+Name+" MCP server", func(ctx context.Context) error { return run(ctx, "codex", "mcp", "remove", Name) })
		} else {
			notes = append(notes, codexConfig+" registers a "+Name+" server, but the codex CLI is not on PATH: run `codex mcp remove "+Name+"`")
		}
	}
	lifecycle := func(label, file, client string) {
		if fileContains(file, lifecycleMarker(client)) {
			add(label+" lifecycle hooks in "+file, func(context.Context) error { _, err := RemoveLifecycleHooks(file, client); return err })
		}
	}
	lifecycle("Codex", filepath.Join(o.Home, ".codex", "hooks.json"), "codex")
	lifecycle("Cursor", filepath.Join(o.Home, ".cursor", "hooks.json"), "cursor")

	for _, j := range []struct {
		label, file string
		path        []string
	}{
		{"Cursor", filepath.Join(o.Home, ".cursor", "mcp.json"), []string{"mcpServers", Name}},
		{"Gemini CLI", filepath.Join(o.Home, ".gemini", "settings.json"), []string{"mcpServers", Name}},
		{"OpenCode", filepath.Join(o.Home, ".config", "opencode", "opencode.json"), []string{"mcp", Name}},
		{"Project", filepath.Join(o.Dir, ".mcp.json"), []string{"mcpServers", Name}},
	} {
		if jsonHasPath(j.file, j.path...) {
			add(fmt.Sprintf("%s: %s entry in %s", j.label, Name, j.file), func(context.Context) error { return removeJSONPath(j.file, j.path...) })
		}
	}

	for _, f := range []string{filepath.Join(o.Dir, "AGENTS.md"), filepath.Join(o.Dir, "CLAUDE.md")} {
		if fileContains(f, blockStart) {
			add("memory block in "+f, func(context.Context) error { return removeBlock(f) })
		}
	}

	if fileContains(claudeSettings, claudeCapKey) || fileContains(codexConfig, codexCapKey) {
		notes = append(notes, "left the context cap ("+claudeCapKey+" / "+codexCapKey+") alone: `mcp install` kept a value you set yourself, so it cannot tell whose it is")
	}
	if !o.KeepToken && o.TokenFile != "" {
		if _, err := os.Stat(o.TokenFile); err == nil {
			add("stored key "+o.TokenFile, func(context.Context) error { return os.Remove(o.TokenFile) })
		}
	}
	return actions, notes
}

func lifecycleMarker(client string) string { return " hook lifecycle --client " + client + " " }

func fileContains(file, s string) bool {
	data, err := os.ReadFile(file) //nolint:gosec // fixed agent config paths under home or the project
	return err == nil && strings.Contains(string(data), s)
}

// hasGateway reports whether a Claude Code settings file carries our gateway header.
func hasGateway(file string) bool {
	doc, _, err := readSettings(file)
	if err != nil {
		return false
	}
	env, _ := doc["env"].(map[string]any)
	hdrs, _ := env[gatewayHeaderKey].(string)
	_, ours := otherHeaders(hdrs)
	return ours
}

func jsonHasPath(file string, path ...string) bool {
	data, err := os.ReadFile(file) //nolint:gosec // fixed agent config paths under home or the project
	if err != nil {
		return false
	}
	var node any
	if json.Unmarshal(data, &node) != nil {
		return false
	}
	for _, k := range path {
		m, ok := node.(map[string]any)
		if !ok {
			return false
		}
		if node, ok = m[k]; !ok {
			return false
		}
	}
	return true
}

// removeJSONPath deletes the key at path, keeping everything else; mergeJSON
// writes the .bak copy.
func removeJSONPath(file string, path ...string) error {
	data, err := os.ReadFile(file) //nolint:gosec // fixed agent config paths under home or the project
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	root := map[string]any{}
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("%s is not plain JSON (comments?); remove the %s entry by hand: %w", file, Name, err)
	}
	node := root
	for _, k := range path[:len(path)-1] {
		child, ok := node[k].(map[string]any)
		if !ok {
			return nil
		}
		node = child
	}
	if _, ok := node[path[len(path)-1]]; !ok {
		return nil
	}
	delete(node, path[len(path)-1])
	return mergeJSON(file, nil, root)
}

// RemoveLifecycleHooks deletes the handlers InstallLifecycleHooks added for
// client from a Codex or Cursor hooks file, keeping every other hook. It
// returns how many it removed.
func RemoveLifecycleHooks(file, client string) (int, error) {
	data, err := os.ReadFile(file) //nolint:gosec // fixed agent config paths under home
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	doc := map[string]any{}
	if err := json.Unmarshal(data, &doc); err != nil {
		return 0, fmt.Errorf("%s is not plain JSON (comments?); remove the CodeSynapse hooks by hand: %w", file, err)
	}
	marker := lifecycleMarker(client)
	ours := func(h any) bool {
		m, _ := h.(map[string]any)
		cmd, _ := m["command"].(string)
		return strings.Contains(cmd, marker)
	}
	hooks, _ := doc["hooks"].(map[string]any)
	removed := 0
	for event, v := range hooks {
		groups, _ := v.([]any)
		var kept []any
		for _, g := range groups {
			m, ok := g.(map[string]any)
			if !ok {
				kept = append(kept, g)
				continue
			}
			if ours(m) { // Cursor: the handler is the entry
				removed++
				continue
			}
			if handlers, ok := m["hooks"].([]any); ok { // Codex: handlers sit in a group
				left := slices.DeleteFunc(slices.Clone(handlers), ours)
				removed += len(handlers) - len(left)
				if len(left) == 0 && len(handlers) > 0 {
					continue
				}
				m["hooks"] = left
			}
			kept = append(kept, m)
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

// removeBlock takes the memory block out of an instructions file. A file that
// held nothing else was created by the installer and goes with it.
func removeBlock(p string) error {
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", p)
	}
	old, err := os.ReadFile(p) //nolint:gosec // path built from the project directory
	if err != nil {
		return err
	}
	s := string(old)
	i, j := strings.Index(s, blockStart), strings.Index(s, blockEnd)
	if i < 0 || j < i {
		return nil
	}
	end := j + len(blockEnd)
	var parts []string
	for _, part := range []string{strings.TrimRight(s[:i], "\n"), strings.Trim(s[end:], "\n")} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return os.Remove(p)
	}
	return os.WriteFile(p, []byte(strings.Join(parts, "\n\n")+"\n"), 0o644) //nolint:gosec // a repository file meant to be committed
}
