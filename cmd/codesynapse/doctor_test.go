package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/agentcfg"
	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// withVersion runs the test as a CLI of this version.
func withVersion(t *testing.T, v string) {
	t.Helper()
	old := version
	version = v
	t.Cleanup(func() { version = old })
}

// fakeHome gives the test an empty home, config folder and PATH, so no
// agent of the machine running the tests is found or touched.
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("PATH", t.TempDir())
	t.Setenv(apiTokenEnv, "")
	t.Setenv(agentcfg.TokenEnv, "")
	return home
}

// fakeAPI answers the version and, for keyAccepted, the rules.
func fakeAPI(t *testing.T, serverVersion, keyAccepted string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/version":
			assert.Equal(t, r.Header.Get("Authorization"), "", "the version is asked without the key")
			_, _ = io.WriteString(w, `{"version":"`+serverVersion+`"}`)
		case "/api/v1/agent-rules":
			if r.Header.Get("Authorization") != "Bearer "+keyAccepted {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"text":"x","server_version":"`+serverVersion+`"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runDoctor(t *testing.T, apiURL string, fix bool) ([]doctorCheck, string, error) {
	t.Helper()
	var out bytes.Buffer
	o := &doctorOptions{EC: &app.Env{Stdout: &out}, apiURL: apiURL, url: "http://127.0.0.1:1/mcp", fix: fix}
	err := o.Run(context.Background(), nil)
	return o.checks(context.Background(), os.Getenv("HOME"), t.TempDir()), out.String(), err
}

func byName(checks []doctorCheck) map[string]doctorCheck {
	m := map[string]doctorCheck{}
	for _, c := range checks {
		m[c.Name] = c
	}
	return m
}

func TestDoctor(t *testing.T) {
	t.Run("current", func(t *testing.T) {
		fakeHome(t)
		withVersion(t, "v0.0.24")
		t.Setenv(apiTokenEnv, "csk_good")
		srv := fakeAPI(t, "v0.0.24", "csk_good")
		checks, text, err := runDoctor(t, srv.URL, false)
		require.NoError(t, err, text)
		for _, c := range checks {
			assert.True(t, c.OK, c.Name+": "+c.Detail)
		}
		assert.Contains(t, text, "CodeSynapse doctor (CLI v0.0.24)")
		assert.Contains(t, text, "present (CODESYNAPSE_API_TOKEN) and accepted")
		assert.NotContains(t, text, "csk_good", "the key is never printed")
	})

	t.Run("outdated CLI and rejected key", func(t *testing.T) {
		fakeHome(t)
		withVersion(t, "v0.0.23")
		t.Setenv(apiTokenEnv, "csk_old")
		srv := fakeAPI(t, "v0.0.24", "csk_new")
		checks, text, err := runDoctor(t, srv.URL, false)
		assert.ErrorIs(t, err, errNeedsAction)
		got := byName(checks)
		assert.False(t, got["CLI version"].OK)
		assert.Contains(t, text, "You are on an old version of CodeSynapse (v0.0.23; the newest is v0.0.24). Please upgrade to the newest version of CodeSynapse, then run `codesynapse doctor --fix`")
		assert.False(t, got["API token"].OK)
		assert.Contains(t, got["API token"].Detail, "rejected by the server")
		assert.Contains(t, text, "NEEDS ACTION")
		assert.Contains(t, text, "Run `codesynapse doctor --fix`")
		assert.NotContains(t, text, "csk_old")
	})

	t.Run("dev build never nags", func(t *testing.T) {
		fakeHome(t)
		withVersion(t, "v0.0.23-4-gabc1234")
		t.Setenv(apiTokenEnv, "csk_good")
		srv := fakeAPI(t, "v0.0.24", "csk_good")
		checks, _, err := runDoctor(t, srv.URL, false)
		require.NoError(t, err)
		assert.Contains(t, byName(checks)["CLI version"].Detail, "development build")
	})

	t.Run("server down and no key", func(t *testing.T) {
		fakeHome(t)
		withVersion(t, "v0.0.24")
		checks, _, err := runDoctor(t, "http://127.0.0.1:1", false)
		assert.ErrorIs(t, err, errNeedsAction)
		got := byName(checks)
		assert.False(t, got["API server"].OK)
		assert.True(t, got["CLI version"].OK, "nothing to compare with")
		assert.Equal(t, got["API token"].Detail, "missing")
	})

	t.Run("token file readable by others is fixed", func(t *testing.T) {
		fakeHome(t)
		withVersion(t, "v0.0.24")
		file, err := storeToken("csk_good")
		require.NoError(t, err)
		require.NoError(t, os.Chmod(file, 0o644)) //nolint:gosec // the case under test
		srv := fakeAPI(t, "v0.0.24", "csk_good")
		checks, _, err := runDoctor(t, srv.URL, false)
		assert.ErrorIs(t, err, errNeedsAction)
		assert.Equal(t, byName(checks)["API token"].Action, "chmod 600 "+file)

		checks, text, err := runDoctor(t, srv.URL, true)
		require.NoError(t, err, text)
		assert.Contains(t, text, "FIXED")
		st, err := os.Stat(file)
		require.NoError(t, err)
		assert.Equal(t, st.Mode().Perm(), os.FileMode(0o600))
		assert.True(t, byName(checks)["API token"].OK, "accepted once the file is private")
	})

	t.Run("plugin behind the CLI", func(t *testing.T) {
		home := fakeHome(t)
		withVersion(t, "v0.0.24")
		t.Setenv(apiTokenEnv, "csk_good")
		srv := fakeAPI(t, "v0.0.24", "csk_good")
		root := installPlugin(t, home, "hook webfetch")
		checks, text, err := runDoctor(t, srv.URL, false)
		assert.ErrorIs(t, err, errNeedsAction)
		c := byName(checks)["Claude Code plugin"]
		assert.False(t, c.OK)
		assert.Contains(t, c.Detail, "1 hook(s) missing: PreToolUse (WebFetch): codesynapse hook webfetch")
		assert.Contains(t, text, "Run `codesynapse doctor --fix`, or update it with /plugin")

		writePluginHooks(t, root, agentcfg.ShippedHooks)
		checks, _, err = runDoctor(t, srv.URL, false)
		require.NoError(t, err)
		assert.True(t, byName(checks)["Claude Code plugin"].OK)
	})

	t.Run("json", func(t *testing.T) {
		fakeHome(t)
		withVersion(t, "v0.0.24")
		t.Setenv(apiTokenEnv, "csk_good")
		srv := fakeAPI(t, "v0.0.24", "csk_good")
		var out bytes.Buffer
		o := &doctorOptions{EC: &app.Env{Stdout: &out, Output: app.OutputJSON}, apiURL: srv.URL}
		require.NoError(t, o.Run(context.Background(), nil))
		var got []doctorCheck
		require.NoError(t, json.Unmarshal(out.Bytes(), &got))
		assert.Len(t, got, 3)
	})
}

// installPlugin records a user-scope plugin in Claude Code's list whose
// hooks.json lacks the hooks with these arguments, and returns its folder.
func installPlugin(t *testing.T, home string, without ...string) string {
	t.Helper()
	root := filepath.Join(home, ".claude", "plugins", "cache", "codesynapse", "codesynapse", "abc")
	var hooks []agentcfg.PluginHook
	for _, h := range agentcfg.ShippedHooks {
		if !strings.Contains(strings.Join(without, "\n"), h.Args) {
			hooks = append(hooks, h)
		}
	}
	writePluginHooks(t, root, hooks)
	list, err := json.Marshal(map[string]any{"version": 2, "plugins": map[string]any{
		"codesynapse@codesynapse": []map[string]string{{"scope": "user", "installPath": root, "version": "abc"}},
	}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(home, ".claude", "plugins", "installed_plugins.json"), list, 0o600))
	return root
}

func writePluginHooks(t *testing.T, root string, hooks []agentcfg.PluginHook) {
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
		doc["hooks"][h.Event] = append(doc["hooks"][h.Event], group{Matcher: h.Matcher, Hooks: []handler{{"command", "codesynapse", strings.Fields(h.Args)}}})
	}
	b, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "hooks"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "hooks", "hooks.json"), b, 0o600))
}

func TestSessionRulesTellAnOutdatedCLI(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	t.Setenv("CODESYNAPSE_API_TOKEN", "csk_test")
	t.Setenv("CLAUDE_PLUGIN_ROOT", "")
	srv := fakeAPI(t, "v0.0.24", "csk_test")
	t.Setenv("CODESYNAPSE_API_URL", srv.URL)

	for _, tc := range []struct {
		name, cli string
		want      string // "" for no notice
	}{
		{"behind", "v0.0.23", "You are on an old version of CodeSynapse (v0.0.23; the newest is v0.0.24). Please upgrade to the newest version of CodeSynapse, then run `codesynapse doctor --fix` and start a new session to pick up the missing changes from this update."},
		{"current", "v0.0.24", ""},
		{"newer", "v0.0.25", ""},
		{"dev", "dev", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withVersion(t, tc.cli)
			out := sessionRules(context.Background(), toolHookInput{}, logger)
			text := out["hookSpecificOutput"].(map[string]any)["additionalContext"].(string)
			msg, _ := out["systemMessage"].(string)
			if tc.want == "" {
				assert.NotContains(t, text, "update notice")
				assert.Equal(t, msg, "")
				return
			}
			assert.Contains(t, text, "x\n\nCodeSynapse update notice: before your first answer in this session, tell the user this in these words: \""+tc.want+"\"")
			assert.Equal(t, msg, "CodeSynapse: "+tc.want)
		})
	}
}

func TestUpdateNoticeComparesThePluginHooks(t *testing.T) {
	root := t.TempDir()
	assert.Equal(t, updateNotice("v0.0.24", "v0.0.24", root), "", "an unreadable plugin says nothing")

	writePluginHooks(t, root, agentcfg.ShippedHooks[1:])
	assert.Equal(t, updateNotice("v0.0.24", "", root), "The CodeSynapse plugin for Claude Code is older than your codesynapse CLI (v0.0.24): 1 hook(s) from this update are missing. Please run `codesynapse doctor --fix` to fix the missing changes from this update, then start a new session.")
	assert.Equal(t, updateNotice("dev", "", root), "", "a dev build never nags")

	writePluginHooks(t, root, append(append([]agentcfg.PluginHook{}, agentcfg.ShippedHooks...), agentcfg.PluginHook{Event: "Notification", Args: "hook notify"}))
	assert.Contains(t, updateNotice("v0.0.24", "", root), "You are on an old version of CodeSynapse: the Claude Code plugin runs hooks your codesynapse CLI (v0.0.24) does not have.")

	writePluginHooks(t, root, agentcfg.ShippedHooks)
	assert.Equal(t, updateNotice("v0.0.24", "v0.0.24", root), "")
	assert.Contains(t, updateNotice("v0.0.23", "v0.0.24", root), "You are on an old version of CodeSynapse (v0.0.23", "the server's release comes first")
}

func TestWithUpgradeNotice(t *testing.T) {
	init := func(server, instructions string) []byte {
		return []byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"codesynapse","version":"` + server + `"}` + instructions + `}}`)
	}
	read := func(b []byte) string {
		var m struct {
			Result struct {
				Instructions string `json:"instructions"`
				Protocol     string `json:"protocolVersion"`
			} `json:"result"`
		}
		require.NoError(t, json.Unmarshal(b, &m))
		assert.Equal(t, m.Result.Protocol, "2025-06-18")
		return m.Result.Instructions
	}
	got := read(withUpgradeNotice(init("v0.0.24", `,"instructions":"Use the graph."`), "v0.0.23"))
	assert.Contains(t, got, "Use the graph.\n\nCodeSynapse update notice:")
	assert.Contains(t, got, "You are on an old version of CodeSynapse (v0.0.23; the newest is v0.0.24).")
	assert.Contains(t, read(withUpgradeNotice(init("v0.0.24", ""), "v0.0.23")), "update notice")

	for _, tc := range []struct{ server, cli string }{{"v0.0.24", "v0.0.24"}, {"v0.0.24", "dev"}, {"dev", "v0.0.23"}} {
		in := init(tc.server, `,"instructions":"Use the graph."`)
		assert.Equal(t, string(withUpgradeNotice(in, tc.cli)), string(in), tc.server+" "+tc.cli)
	}
	for _, in := range []string{`not json`, `{"id":1,"error":{"code":1}}`, `{"result":[]}`, `{"result":{"serverInfo":{"version":"v9.0.0"},"instructions":5}}`} {
		assert.Equal(t, string(withUpgradeNotice([]byte(in), "v0.0.1")), in)
	}
}

func FuzzWithUpgradeNotice(f *testing.F) {
	f.Add([]byte(`{"result":{"serverInfo":{"version":"v0.0.24"},"instructions":"x"}}`), "v0.0.23")
	f.Add([]byte(`{"result":{}}`), "dev")
	f.Fuzz(func(t *testing.T, data []byte, cli string) {
		out := withUpgradeNotice(data, cli)
		if !bytes.Equal(out, data) && !json.Valid(out) {
			t.Fatalf("changed answer is not JSON: %q", out)
		}
	})
}
