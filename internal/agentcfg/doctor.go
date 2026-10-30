package agentcfg

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// RegState is how an agent's CodeSynapse registration looks.
type RegState string

const (
	// RegOK is a registration as `mcp install` writes it.
	RegOK RegState = "ok"
	// RegMissing is an agent that needs a registration and has none.
	RegMissing RegState = "missing"
	// RegStale is a registration `mcp install` would write differently now.
	RegStale RegState = "stale"
	// RegAbsent is an agent without a registration that does not need one.
	RegAbsent RegState = "absent"
)

// Registration is what Check found for one agent of Detect.
type Registration struct {
	ID     string   `json:"id"`
	State  RegState `json:"state"`
	Detail string   `json:"detail"`
	// URL and APIURL are the endpoints the registration names, so a repair
	// keeps them.
	URL    string `json:"-"`
	APIURL string `json:"-"`
}

// NeedsAction reports whether `mcp install` should run for the agent.
func (r Registration) NeedsAction() bool { return r.State == RegMissing || r.State == RegStale }

// Check reads what `mcp install` wrote for each agent Detect finds, read
// only. Claude Code needs the plugin or a user-scope server; the other
// agents are optional and only checked when they have an entry.
func Check(home, cwd string) []Registration {
	var out []Registration
	if _, err := exec.LookPath("claude"); err == nil {
		out = append(out, checkClaude(home, cwd))
	}
	if _, err := exec.LookPath("codex"); err == nil {
		out = append(out, checkCodex(filepath.Join(home, ".codex", "config.toml")))
	}
	if dirExists(filepath.Join(home, ".gemini")) {
		out = append(out, checkURLServer("gemini", filepath.Join(home, ".gemini", "settings.json"), []string{"mcpServers", Name}, "httpUrl"))
	}
	if dirExists(filepath.Join(home, ".config", "opencode")) {
		out = append(out, checkURLServer("opencode", filepath.Join(home, ".config", "opencode", "opencode.json"), []string{"mcp", Name}, "url"))
	}
	if dirExists(filepath.Join(home, ".cursor")) {
		out = append(out, checkStdio("cursor", filepath.Join(home, ".cursor", "mcp.json"), "cursor"))
	}
	if r := checkStdio("project", filepath.Join(cwd, ".mcp.json"), ""); r.State != RegAbsent {
		out = append(out, r) // a project without CodeSynapse is not a finding
	}
	return out
}

func checkClaude(home, cwd string) Registration {
	r := Registration{ID: "claude"}
	if ClaudePluginEnabled(home, cwd) {
		var left []string
		if claudeUserServer(home) {
			left = append(left, "a user-scope server (tools would run twice)")
		}
		for _, f := range []string{filepath.Join(home, ".claude", "settings.json"), filepath.Join(cwd, ".claude", "settings.local.json")} {
			if hasClaudeHooks(f) {
				left = append(left, "hooks in "+f)
			}
		}
		if len(left) > 0 {
			r.State, r.Detail = RegStale, "the plugin is enabled, but the older setup it replaces is still there: "+strings.Join(left, ", ")
			return r
		}
		r.State, r.Detail = RegOK, "the plugin brings the bridge and the hooks"
		return r
	}
	var doc struct {
		MCPServers map[string]stdioEntry `json:"mcpServers"`
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude.json")) //nolint:gosec // Claude Code's own config file
	if err != nil || json.Unmarshal(data, &doc) != nil {
		r.State, r.Detail = RegMissing, "no plugin and no "+Name+" server registered"
		return r
	}
	e, ok := doc.MCPServers[Name]
	if !ok {
		r.State, r.Detail = RegMissing, "no plugin and no "+Name+" server registered"
		return r
	}
	return e.check(r, "")
}

// stdioEntry is a registration that starts `codesynapse mcp bridge`.
type stdioEntry struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	URL     string   `json:"url"`
}

func (e stdioEntry) check(r Registration, client string) Registration {
	r.URL, r.APIURL = flagValue(e.Args, "--url"), flagValue(e.Args, "--api-url")
	switch {
	case e.Command == "":
		r.State, r.Detail = RegStale, "the registration is not the local bridge (an older remote setup)"
	case !commandExists(e.Command):
		r.State, r.Detail = RegStale, "the registered binary "+e.Command+" no longer exists"
	case len(e.Args) < 2 || e.Args[0] != "mcp" || e.Args[1] != "bridge":
		r.State, r.Detail = RegStale, "the registration does not start the bridge"
	case client != "" && flagValue(e.Args, "--client") != client:
		r.State, r.Detail = RegStale, "the bridge is not set up for "+client
	default:
		r.State, r.Detail = RegOK, "starts "+e.Command+" mcp bridge"
	}
	return r
}

// flagValue is the value after flag in args, or "".
func flagValue(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func commandExists(cmd string) bool {
	if filepath.IsAbs(cmd) {
		st, err := os.Stat(cmd)
		return err == nil && !st.IsDir()
	}
	_, err := exec.LookPath(cmd)
	return err == nil
}

func checkStdio(id, file, client string) Registration {
	r := Registration{ID: id}
	var doc struct {
		MCPServers map[string]stdioEntry `json:"mcpServers"`
	}
	data, err := os.ReadFile(file) //nolint:gosec // fixed paths under home or the project
	if err != nil || json.Unmarshal(data, &doc) != nil {
		r.State, r.Detail = RegAbsent, "not registered"
		return r
	}
	e, ok := doc.MCPServers[Name]
	if !ok {
		r.State, r.Detail = RegAbsent, "not registered"
		return r
	}
	return e.check(r, client)
}

func checkURLServer(id, file string, path []string, key string) Registration {
	r := Registration{ID: id, State: RegAbsent, Detail: "not registered"}
	var node any
	data, err := os.ReadFile(file) //nolint:gosec // fixed paths under home
	if err != nil || json.Unmarshal(data, &node) != nil {
		return r
	}
	for _, p := range path {
		m, ok := node.(map[string]any)
		if !ok {
			return r
		}
		node = m[p]
	}
	m, ok := node.(map[string]any)
	if !ok {
		return r
	}
	if u, _ := m[key].(string); u != "" {
		r.State, r.Detail, r.URL = RegOK, "connects to "+u, u
		return r
	}
	r.State, r.Detail = RegStale, "the entry has no server address"
	return r
}

// checkCodex reads the [mcp_servers.codesynapse] table `codex mcp add`
// writes. Only the keys needed here are read: command and args.
func checkCodex(file string) Registration {
	r := Registration{ID: "codex", State: RegAbsent, Detail: "not registered"}
	f, err := os.Open(file) //nolint:gosec // fixed path under home
	if err != nil {
		return r
	}
	defer f.Close() //nolint:errcheck // read-only
	var e stdioEntry
	in := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			if in {
				break
			}
			in = line == "[mcp_servers."+Name+"]"
			continue
		}
		if !in {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.TrimSpace(key) {
		case "command":
			e.Command, _ = strconv.Unquote(val) //nolint:errcheck // unquotable means none
		case "args":
			_ = json.Unmarshal([]byte(val), &e.Args) //nolint:errcheck // TOML string arrays are JSON arrays
		}
		e.Type = "stdio"
	}
	if e.Type == "" {
		return r
	}
	return e.check(r, "codex")
}
