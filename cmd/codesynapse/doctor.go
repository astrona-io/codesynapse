package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/astrona-io/codesynapse/internal/agentcfg"
	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/cliversion"
	"github.com/astrona-io/codesynapse/internal/gatewayproxy"
	"github.com/astrona-io/codesynapse/internal/toolsets"
)

// doctorTimeout bounds each request doctor makes to the API.
const doctorTimeout = 5 * time.Second

// doctorOptions configures `codesynapse doctor`.
type doctorOptions struct {
	EC     *app.Env
	fix    bool
	url    string
	apiURL string
}

// doctorCheck is one line of the report. Action is the command or step
// that repairs it; Fixed says --fix repaired it.
type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Action string `json:"action,omitempty"`
	Fixed  bool   `json:"fixed,omitempty"`

	fix func(ctx context.Context) error // nil: nothing --fix can do
}

func (o *doctorOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	f := o.EC.Command.Flags()
	f.BoolVar(&o.fix, "fix", false, "Repair what is safe to repair here: the MCP registrations `mcp install` writes, the Claude Code plugin (through Claude Code's own plugin update), the token file's permissions and the gateway's failover proxy")
	f.StringVar(&o.url, "url", app.Getenv("CODESYNAPSE_MCP_URL", "http://localhost:8090/mcp"), "MCP endpoint for an agent that has no registration yet")
	f.StringVar(&o.apiURL, "api-url", app.Getenv("CODESYNAPSE_API_URL", "http://127.0.0.1:8080"), "API to check")
	return nil
}

func (o *doctorOptions) Complete(context.Context, *app.Env) error { return nil }
func (o *doctorOptions) Validate(context.Context, *app.Env) error { return nil }

// errNeedsAction makes doctor exit non-zero, so scripts and CI can use it.
var errNeedsAction = errors.New("some checks need action; see above")

func (o *doctorOptions) Run(ctx context.Context, _ *app.Env) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	checks := o.checks(ctx, home, cwd)
	fixed := false
	if o.fix {
		for i := range checks {
			c := &checks[i]
			if c.OK || c.fix == nil {
				continue
			}
			if err := c.fix(ctx); err != nil {
				c.Detail += " (fix failed: " + err.Error() + ")"
				continue
			}
			c.OK, c.Fixed, fixed = true, true, true
		}
	}
	if o.EC.Output == app.OutputJSON {
		enc := json.NewEncoder(o.EC.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(checks); err != nil {
			return err
		}
	} else {
		writeDoctor(o.EC.Stdout, checks, o.fix)
		if fixed {
			_, _ = fmt.Fprintln(o.EC.Stdout, "\nStart a new session in your agents (and restart Claude Code after a plugin update) to pick up the changes.")
		}
	}
	if slices.ContainsFunc(checks, func(c doctorCheck) bool { return !c.OK }) {
		return errNeedsAction
	}
	return nil
}

func writeDoctor(w io.Writer, checks []doctorCheck, fixing bool) {
	_, _ = fmt.Fprintf(w, "CodeSynapse doctor (CLI %s)\n", version)
	needs := false
	for _, c := range checks {
		state := "OK"
		switch {
		case c.Fixed:
			state = "FIXED"
		case !c.OK:
			state, needs = "NEEDS ACTION", true
		}
		_, _ = fmt.Fprintf(w, "  %-13s %-20s %s\n", state, c.Name, c.Detail)
		if !c.OK && c.Action != "" {
			_, _ = fmt.Fprintf(w, "  %-13s %-20s → %s\n", "", "", c.Action)
		}
	}
	if needs && !fixing {
		_, _ = fmt.Fprintf(w, "\nRun `%s` to repair what can be repaired here.\n", cliversion.FixCommand)
	}
}

// checks runs every check, read only; the fix of each runs later.
func (o *doctorOptions) checks(ctx context.Context, home, cwd string) []doctorCheck {
	server, serverVersion, serverOK := o.serverCheck(ctx)
	out := []doctorCheck{server, o.versionCheck(serverVersion, serverOK), o.tokenCheck(ctx, serverOK)}
	out = append(out, o.registrationChecks(home, cwd)...)
	if c, ok := pluginCheck(home); ok {
		out = append(out, c)
	}
	if files, err := gatewayproxy.DefaultFiles(); err == nil {
		out = append(out, gatewayCheck(ctx, filepath.Join(home, ".claude", "settings.json"), files, setUpFailover, startProxy))
	}
	return out
}

// serverCheck asks the API for its version, which is also the newest CLI.
func (o *doctorOptions) serverCheck(ctx context.Context) (c doctorCheck, serverVersion string, ok bool) {
	c = doctorCheck{Name: "API server"}
	ctx, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	api := newAPIClient(o.apiURL)
	api.token = "" // public route; the token is checked on its own
	var res struct {
		Version string `json:"version"`
	}
	if err := api.do(ctx, http.MethodGet, "/api/v1/version", nil, &res); err != nil {
		c.Detail = "no answer from " + o.apiURL
		c.Action = "Start CodeSynapse, or set CODESYNAPSE_API_URL to its API"
		return c, "", false
	}
	c.OK = true
	c.Detail = o.apiURL + " answers (server " + cmp.Or(res.Version, "version unknown") + ")"
	return c, res.Version, true
}

func (o *doctorOptions) versionCheck(serverVersion string, serverOK bool) doctorCheck {
	c := doctorCheck{Name: "CLI version", OK: true}
	switch {
	case !cliversion.IsRelease(version):
		c.Detail = version + " is a development build; not compared"
	case !serverOK || !cliversion.IsRelease(serverVersion):
		c.Detail = version + " (the server's release is unknown, so not compared)"
	case cliversion.Behind(version, serverVersion):
		c.OK = false
		c.Detail = cliversion.UpgradeNotice(version, serverVersion)
		c.Action = "Upgrade to the newest version of CodeSynapse (" + serverVersion + "), then run `" + cliversion.FixCommand + "` and start a new session"
	default:
		c.Detail = version + ", the same as the server or newer"
	}
	return c
}

func (o *doctorOptions) tokenCheck(ctx context.Context, serverOK bool) doctorCheck {
	c := doctorCheck{Name: "API token"}
	api := newAPIClient(o.apiURL)
	source := apiTokenEnv
	file := ""
	if os.Getenv(apiTokenEnv) == "" {
		file, _ = tokenFile() //nolint:errcheck // no config folder means no file
		source = file
	}
	if api.token == "" {
		c.Detail = "missing"
		c.Action = "Create a personal key in the dashboard (Settings → API keys), put it in " + agentcfg.TokenEnv + " and run `codesynapse mcp install`"
		return c
	}
	// The token itself is never printed: only where it came from and how it fares.
	if file != "" && runtime.GOOS != "windows" {
		if st, err := os.Stat(file); err == nil && st.Mode().Perm()&0o077 != 0 {
			c.Detail = "present in " + file + ", but other users can read the file"
			c.Action = "chmod 600 " + file
			c.fix = func(context.Context) error { return os.Chmod(file, 0o600) }
			return c
		}
	}
	if !strings.HasPrefix(api.token, "csk_") {
		c.Detail = "present (" + source + "), but not a personal key (csk_…); the bridge and the hooks need one"
		c.Action = "Create a personal key in the dashboard (Settings → API keys), put it in " + agentcfg.TokenEnv + " and run `codesynapse mcp install`"
		return c
	}
	if !serverOK {
		c.OK, c.Detail = true, "present ("+source+"); not checked, the server does not answer"
		return c
	}
	ctx, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	// for=setup: a check, not a session starting, so it is not counted.
	err := api.do(ctx, http.MethodGet, "/api/v1/agent-rules?for=setup", nil, nil)
	var se *statusError
	switch {
	case err == nil:
		c.OK, c.Detail = true, "present ("+source+") and accepted"
	case errors.As(err, &se) && (se.Status == http.StatusUnauthorized || se.Status == http.StatusForbidden):
		c.Detail = "present (" + source + "), but rejected by the server"
		c.Action = "Create a new personal key in the dashboard (Settings → API keys), put it in " + agentcfg.TokenEnv + " and run `codesynapse mcp install`"
	default:
		c.OK, c.Detail = true, "present ("+source+"); the server could not check it now"
	}
	return c
}

func (o *doctorOptions) registrationChecks(home, cwd string) []doctorCheck {
	var out []doctorCheck
	for _, r := range agentcfg.Check(home, cwd) {
		c := doctorCheck{Name: "MCP: " + r.ID, OK: !r.NeedsAction(), Detail: string(r.State) + ": " + r.Detail}
		if r.State == agentcfg.RegAbsent {
			c.Detail = "not registered (optional)"
		}
		if !c.OK {
			c.Action = "codesynapse mcp install --only " + r.ID
			c.fix = func(ctx context.Context) error { return o.reinstall(ctx, home, cwd, r) }
		}
		out = append(out, c)
	}
	return out
}

// reinstall runs what `mcp install --only <id> --yes` runs for one agent,
// keeping the endpoints the old registration named. It leaves the context
// cap as it is.
func (o *doctorOptions) reinstall(ctx context.Context, home, cwd string, r agentcfg.Registration) error {
	mcpURL := r.URL
	if mcpURL == "" {
		sel, err := toolsets.Parse(toolsets.Core)
		if err != nil {
			return err
		}
		if mcpURL, err = withToolsets(o.url, sel); err != nil {
			return err
		}
	}
	server, err := installServer(mcpURL, cmp.Or(r.APIURL, o.apiURL))
	if err != nil {
		return err
	}
	for _, t := range agentcfg.Detect(home, cwd) {
		if t.ID == r.ID {
			return t.Install(ctx, server)
		}
	}
	return errors.New(r.ID + " is no longer detected")
}

// pluginCheck compares the Claude Code plugin's hooks with the ones this
// CLI ships. ok is false when the plugin is not installed.
func pluginCheck(home string) (doctorCheck, bool) {
	p, found := agentcfg.FindClaudePlugin(home)
	if !found {
		return doctorCheck{}, false
	}
	c := doctorCheck{Name: "Claude Code plugin"}
	update := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if err := agentcfg.UpdateClaudePlugin(ctx, p); err != nil {
			return err
		}
		if c, ok := pluginCheck(home); ok && !c.OK {
			return errors.New("still behind after the update: " + c.Detail)
		}
		return nil
	}
	installed, err := agentcfg.ReadPluginHooks(p.Path)
	if err != nil {
		c.Detail = "installed (" + p.Key + "), but its hooks cannot be read"
		c.Action = "Update or reinstall the plugin with /plugin in Claude Code, then start a new session"
		c.fix = update
		return c, true
	}
	missing, extra := agentcfg.CompareHooks(installed)
	switch {
	case len(extra) > 0:
		c.Detail = fmt.Sprintf("runs %d hook(s) this CLI does not have (%s)", len(extra), extra[0])
		c.Action = "Upgrade to the newest version of CodeSynapse, then run `" + cliversion.FixCommand + "`"
	case len(missing) > 0:
		names := make([]string, 0, len(missing))
		for _, h := range missing {
			names = append(names, h.String())
		}
		c.Detail = fmt.Sprintf("%s is older than this CLI: %d hook(s) missing: %s", p.Key, len(missing), strings.Join(names, "; "))
		c.Action = "Run `" + cliversion.FixCommand + "`, or update it with /plugin in Claude Code; then start a new session"
		c.fix = update
	default:
		c.OK, c.Detail = true, p.Key+" runs every hook of this CLI"
	}
	return c, true
}

func doctorCmd(ec *app.Env) *cobra.Command {
	return app.NewSubCommand("doctor", &doctorOptions{EC: ec}, ec).
		WithShortDesc("Check that this CLI, the server, the agent registrations, the Claude Code plugin and the gateway failover are current; --fix repairs them").
		WithLongDesc("Checks the CLI's version against the server's, that the API answers and accepts your key (never printed), what `codesynapse mcp install` registered with each agent, that the Claude Code plugin runs every hook this CLI ships, and that a gateway set up by `mcp install --gateway` goes through the failover proxy. Each line is OK or NEEDS ACTION with the command that repairs it. --fix runs the repairs that are safe here: `mcp install` again for agents whose registration is missing or stale (keeping their endpoints and your context cap), Claude Code's own plugin update for a user-scope plugin that is behind, chmod 600 on the token file, and the failover proxy for a gateway reached directly or with missing hooks (reusing the gateway URL and key in Claude Code's settings; a setup made with --failover=false is left alone, and no gateway is set up where there was none). It exits non-zero while something needs action. Start a new session afterwards.").
		WithExample("  codesynapse doctor\n  codesynapse doctor --fix\n  codesynapse doctor -o json").
		WithGroupID(app.GroupBase).
		WithNoArgs().
		Build()
}
