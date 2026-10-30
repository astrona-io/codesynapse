package agentcfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// fakeAgents puts executables with these names on an otherwise empty PATH.
func fakeAgents(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\nexit 0\n"), 0o700)) //nolint:gosec // test executable
	}
	t.Setenv("PATH", dir)
	return dir
}

func shippedJSON(t *testing.T, hooks []PluginHook) string {
	t.Helper()
	type handler struct {
		Type    string   `json:"type"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	type group struct {
		Matcher string    `json:"matcher,omitempty"`
		Hooks   []handler `json:"hooks"`
	}
	doc := map[string]map[string][]group{"hooks": {}}
	for _, h := range hooks {
		doc["hooks"][h.Event] = append(doc["hooks"][h.Event], group{Matcher: h.Matcher, Hooks: []handler{{"command", Name, strings.Fields(h.Args)}}})
	}
	b, err := json.Marshal(doc)
	require.NoError(t, err)
	return string(b)
}

func TestCompareHooks(t *testing.T) {
	all, err := ParsePluginHooks([]byte(shippedJSON(t, ShippedHooks)))
	require.NoError(t, err)
	missing, extra := CompareHooks(all)
	assert.Len(t, missing, 0)
	assert.Len(t, extra, 0)

	// An older plugin lacks the webfetch hook; a newer one runs one more.
	var older []PluginHook
	for _, h := range all {
		if h.Args != "hook webfetch" {
			older = append(older, h)
		}
	}
	missing, extra = CompareHooks(older)
	require.Len(t, missing, 1)
	assert.Equal(t, missing[0].String(), "PreToolUse (WebFetch): codesynapse hook webfetch")
	assert.Len(t, extra, 0)

	newer := append(all, PluginHook{Event: "Notification", Args: "hook notify"})
	missing, extra = CompareHooks(newer)
	assert.Len(t, missing, 0)
	require.Len(t, extra, 1)
	assert.Equal(t, extra[0].Args, "hook notify")
}

func TestParsePluginHooks(t *testing.T) {
	got, err := ParsePluginHooks([]byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"codesynapse","args":["hook","stop"]},{"type":"command","command":"other","args":["x"]}]},{"hooks":[{"command":"codesynapse","args":["hook","stop"]}]}]}}`))
	require.NoError(t, err)
	assert.Equal(t, got, []PluginHook{{Event: "Stop", Args: "hook stop"}}, "other commands and duplicates are dropped")
	_, err = ParsePluginHooks([]byte(`{"hooks":`))
	assert.Error(t, err)
	_, err = ParsePluginHooks(make([]byte, maxHooksFile+1))
	assert.Error(t, err)
}

func FuzzParsePluginHooks(f *testing.F) {
	f.Add([]byte(`{"hooks":{"Stop":[{"hooks":[{"command":"codesynapse","args":["hook","stop"]}]}]}}`))
	f.Add([]byte(`{"hooks":{"":[{"matcher":"Read","hooks":[{"command":"codesynapse"}]}]}}`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, data []byte) {
		hooks, err := ParsePluginHooks(data)
		if err != nil {
			return
		}
		missing, extra := CompareHooks(hooks)
		if len(missing) > len(ShippedHooks) || len(extra) > len(hooks) {
			t.Fatalf("compare counts out of range: %d missing, %d extra of %d", len(missing), len(extra), len(hooks))
		}
	})
}

func TestFindClaudePlugin(t *testing.T) {
	home := t.TempDir()
	_, ok := FindClaudePlugin(home)
	assert.False(t, ok, "no file")

	cache := filepath.Join(home, ".claude", "plugins", "cache", "codesynapse", "codesynapse", "abc")
	writeFile(t, filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), `{"version":2,"plugins":{
		"codesynapse@codesynapse":[
			{"scope":"project","installPath":"`+filepath.Join(home, ".claude", "plugins", "cache", "x")+`","version":"p"},
			{"scope":"user","installPath":"`+cache+`","version":"abc"}],
		"codesynapse@bad name":[{"scope":"user","installPath":"`+cache+`"}],
		"codesynapse@elsewhere":[{"scope":"user","installPath":"/etc"}],
		"other@codesynapse":[{"scope":"user","installPath":"`+cache+`"}]}}`)
	p, ok := FindClaudePlugin(home)
	require.True(t, ok)
	assert.Equal(t, p, InstalledPlugin{Key: "codesynapse@codesynapse", Marketplace: "codesynapse", Scope: "user", Path: cache, Version: "abc"})

	// A folder outside Claude Code's plugin folder is never read.
	writeFile(t, filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), `{"plugins":{"codesynapse@m":[{"scope":"user","installPath":"`+filepath.Join(home, ".claude", "plugins", "..", "..", "x")+`"}]}}`)
	_, ok = FindClaudePlugin(home)
	assert.False(t, ok)
}

func TestReadPluginHooks(t *testing.T) {
	root := t.TempDir()
	_, err := ReadPluginHooks(root)
	assert.Error(t, err)
	writeFile(t, filepath.Join(root, "hooks", "hooks.json"), shippedJSON(t, ShippedHooks[:2]))
	got, err := ReadPluginHooks(root)
	require.NoError(t, err)
	assert.Len(t, got, 2)
}

func TestCheck(t *testing.T) {
	bin := filepath.Join(fakeAgents(t, "claude", "codex"), "claude") // any existing file stands in for the binary
	bridge := func(client string) string {
		args := `["mcp","bridge","--url","http://mcp:8090/mcp","--api-url","http://api:8080"`
		if client != "" {
			args += `,"--client","` + client + `"`
		}
		return `{"type":"stdio","command":"` + bin + `","args":` + args + `]}`
	}
	byID := func(rs []Registration) map[string]Registration {
		m := map[string]Registration{}
		for _, r := range rs {
			m[r.ID] = r
		}
		return m
	}

	t.Run("nothing registered", func(t *testing.T) {
		home, cwd := t.TempDir(), t.TempDir()
		got := byID(Check(home, cwd))
		assert.Equal(t, got["claude"].State, RegMissing)
		assert.True(t, got["claude"].NeedsAction())
		assert.Equal(t, got["codex"].State, RegAbsent)
		assert.False(t, got["codex"].NeedsAction(), "optional agents do not nag")
		_, listed := got["project"]
		assert.False(t, listed, "a project without .mcp.json is not listed")
	})

	t.Run("current", func(t *testing.T) {
		home, cwd := t.TempDir(), t.TempDir()
		writeFile(t, filepath.Join(home, ".claude.json"), `{"mcpServers":{"codesynapse":`+bridge("")+`}}`)
		writeFile(t, filepath.Join(home, ".codex", "config.toml"), "[other]\nx = 1\n\n[mcp_servers.codesynapse]\ncommand = \""+bin+"\"\nargs = [\"mcp\", \"bridge\", \"--client\", \"codex\", \"--url\", \"http://mcp:8090/mcp\"]\n\n[mcp_servers.next]\ncommand = \"x\"\n")
		writeFile(t, filepath.Join(cwd, ".mcp.json"), `{"mcpServers":{"codesynapse":{"type":"stdio","command":"claude","args":["mcp","bridge"]}}}`)
		got := byID(Check(home, cwd))
		assert.Equal(t, got["claude"].State, RegOK)
		assert.Equal(t, got["claude"].URL, "http://mcp:8090/mcp")
		assert.Equal(t, got["claude"].APIURL, "http://api:8080")
		assert.Equal(t, got["codex"].State, RegOK, got["codex"].Detail)
		assert.Equal(t, got["project"].State, RegOK, got["project"].Detail)
	})

	t.Run("stale", func(t *testing.T) {
		home, cwd := t.TempDir(), t.TempDir()
		writeFile(t, filepath.Join(home, ".claude.json"), `{"mcpServers":{"codesynapse":{"type":"stdio","command":"/gone/codesynapse","args":["mcp","bridge"]}}}`)
		writeFile(t, filepath.Join(home, ".cursor", "mcp.json"), `{"mcpServers":{"codesynapse":`+bridge("")+`}}`)
		writeFile(t, filepath.Join(home, ".gemini", "settings.json"), `{"mcpServers":{"codesynapse":{"headers":{}}}}`)
		require.NoError(t, os.MkdirAll(filepath.Join(home, ".config", "opencode"), 0o700))
		got := byID(Check(home, cwd))
		assert.Equal(t, got["claude"].State, RegStale)
		assert.Contains(t, got["claude"].Detail, "/gone/codesynapse no longer exists")
		assert.Equal(t, got["cursor"].State, RegStale, "a cursor bridge needs --client cursor")
		assert.Equal(t, got["gemini"].State, RegStale)
		assert.Equal(t, got["opencode"].State, RegAbsent)
	})

	t.Run("plugin", func(t *testing.T) {
		home, cwd := t.TempDir(), t.TempDir()
		writeFile(t, filepath.Join(home, ".claude", "settings.json"), `{"enabledPlugins":{"codesynapse@codesynapse":true}}`)
		assert.Equal(t, byID(Check(home, cwd))["claude"].State, RegOK)

		writeFile(t, filepath.Join(home, ".claude.json"), `{"mcpServers":{"codesynapse":`+bridge("")+`}}`)
		got := byID(Check(home, cwd))["claude"]
		assert.Equal(t, got.State, RegStale)
		assert.Contains(t, got.Detail, "user-scope server")
	})
}
