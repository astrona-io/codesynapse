package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestPluginFiles keeps plugins/codesynapse in step with this CLI: the
// files parse, and every command the hooks and the MCP config run exists.
// (`just plugin-check` runs Claude Code's own validator where it is installed.)
func TestPluginFiles(t *testing.T) {
	root := filepath.Join("..", "..", "plugins", "codesynapse")
	cli := &cobra.Command{Use: "codesynapse"}
	cli.AddGroup(&cobra.Group{ID: app.GroupBase, Title: "Commands"})
	cli.AddCommand(commands(&app.Env{})...)
	exists := func(args []string) {
		t.Helper()
		cmd, rest, err := cli.Find(args)
		require.NoError(t, err, strings.Join(args, " "))
		assert.True(t, cmd != cli && cmd.Runnable(), "codesynapse "+strings.Join(args, " ")+" is a command")
		for _, a := range rest {
			assert.True(t, strings.HasPrefix(a, "-"), "only flags after the command: "+a)
		}
	}
	read := func(name string, v any) {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(root, name))
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, v), name)
	}

	var manifest struct {
		Name    string `json:"name"`
		License string `json:"license"`
	}
	read(".claude-plugin/plugin.json", &manifest)
	assert.Equal(t, manifest.Name, "codesynapse")
	assert.Equal(t, manifest.License, "Apache-2.0")

	var mcp struct {
		Servers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	read(".mcp.json", &mcp)
	require.Len(t, mcp.Servers, 1)
	for _, s := range mcp.Servers {
		assert.Equal(t, s.Command, "codesynapse")
		exists(s.Args)
	}

	var hooks struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string   `json:"type"`
				Command string   `json:"command"`
				Args    []string `json:"args"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	read("hooks/hooks.json", &hooks)
	for _, event := range []string{"SessionStart", "PreCompact", "PreToolUse", "Stop"} {
		require.True(t, len(hooks.Hooks[event]) > 0, event)
		for _, g := range hooks.Hooks[event] {
			for _, h := range g.Hooks {
				assert.Equal(t, h.Type, "command")
				assert.Equal(t, h.Command, "codesynapse")
				exists(h.Args)
			}
		}
	}
	skill, err := os.ReadFile(filepath.Join(root, "skills", "codesynapse", "SKILL.md"))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(skill), "---\nname: codesynapse\ndescription: "), "skill frontmatter")

	var market struct {
		Plugins []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"plugins"`
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", ".claude-plugin", "marketplace.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &market))
	require.Len(t, market.Plugins, 1)
	assert.Equal(t, market.Plugins[0].Source, "./plugins/codesynapse")
}
