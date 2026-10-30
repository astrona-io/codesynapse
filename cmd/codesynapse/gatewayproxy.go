package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/astrona-io/codesynapse/internal/agentcfg"
	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/gatewayproxy"
)

// gatewayProxyOptions configures `codesynapse gateway proxy`.
type gatewayProxyOptions struct {
	EC      *app.Env
	listen  string
	gateway string
	ensure  bool
	idle    time.Duration

	files gatewayproxy.Files
	cfg   gatewayproxy.Config
	// configured is false when install never set the proxy up.
	configured bool
}

func (o *gatewayProxyOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	f := o.EC.Command.Flags()
	f.StringVar(&o.listen, "listen", "", "Loopback address to listen on (default 127.0.0.1 and the port mcp install saved)")
	f.StringVar(&o.gateway, "gateway", "", "Gateway to forward to instead of the saved one (https, or http to this machine)")
	f.BoolVar(&o.ensure, "ensure", false, "Hook mode: start the proxy in the background unless it runs, and point Claude Code back at the gateway when it cannot run. Always exits 0")
	f.DurationVar(&o.idle, "idle", gatewayproxy.DefaultIdle, "Stop after this long without a request; the next session starts it again")
	return nil
}

func (o *gatewayProxyOptions) Complete(context.Context, *app.Env) error {
	files, err := gatewayproxy.DefaultFiles()
	if err != nil {
		if o.ensure {
			return nil
		}
		return err
	}
	o.files = files
	cfg, err := files.Load()
	switch {
	case err == nil:
		o.cfg, o.configured = cfg, true
	case o.ensure:
		return nil // not set up (or unreadable): the hook does nothing
	case errors.Is(err, gatewayproxy.ErrNotConfigured) && o.gateway != "":
		o.cfg = gatewayproxy.Config{Port: gatewayproxy.DefaultPort}
	default:
		return fmt.Errorf("%w: run codesynapse mcp install --gateway", err)
	}
	if o.gateway != "" {
		o.cfg.Gateway = o.gateway
	}
	if o.listen == "" {
		o.listen = net.JoinHostPort("127.0.0.1", strconv.Itoa(o.cfg.Port))
	}
	return nil
}

func (o *gatewayProxyOptions) Validate(context.Context, *app.Env) error {
	if o.ensure {
		return nil
	}
	if err := o.cfg.Validate(); err != nil {
		return err
	}
	if o.idle <= 0 {
		return errors.New("--idle must be positive")
	}
	return gatewayproxy.ValidateListen(o.listen)
}

func (o *gatewayProxyOptions) Run(ctx context.Context, _ *app.Env) error {
	if o.ensure {
		if !o.configured {
			return nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return nil //nolint:nilerr // a hook never fails the session
		}
		ensureProxy(ctx, o.files, o.cfg, filepath.Join(home, ".claude", "settings.json"), startProxy, o.EC.Stdout)
		return nil
	}
	secret, err := o.files.Secret()
	if err != nil {
		return err
	}
	p, err := gatewayproxy.New(gatewayproxy.Options{Config: o.cfg, Files: o.files, Secret: secret, Logger: o.EC.Logger, Idle: o.idle})
	if err != nil {
		return err
	}
	ln, err := gatewayproxy.Listen(ctx, o.listen)
	if err != nil {
		return err
	}
	o.EC.Logger.Info("gateway proxy listening", "addr", ln.Addr().String())
	return p.Run(ctx, ln)
}

// ensureStartWait bounds how long the hook waits for a proxy it started.
const ensureStartWait = 3 * time.Second

// ensureProxy makes sure our proxy answers on the saved port and that
// Claude Code points where requests will be answered: at the proxy when it
// runs, else back at the gateway itself (fail open: a dead ANTHROPIC_BASE_URL
// would stop every request). A port held by another program is never sent the
// key. It never returns an error: it runs as a hook.
func ensureProxy(ctx context.Context, files gatewayproxy.Files, cfg gatewayproxy.Config, settings string, start func() error, out io.Writer) {
	secret, err := files.Secret()
	if err != nil {
		fallBack(settings, cfg, out, "its secret file cannot be read")
		return
	}
	switch waitProxy(ctx, cfg, secret, start) {
	case gatewayproxy.PortOurs:
		_, _ = agentcfg.PointClaudeGateway(settings, cfg.URL(), cfg.Gateway, cfg.URL()) //nolint:errcheck // best effort; the proxy runs
	case gatewayproxy.PortStranger:
		fallBack(settings, cfg, out, "another program holds port "+strconv.Itoa(cfg.Port))
	default:
		fallBack(settings, cfg, out, "it did not start (log: "+files.LogPath()+")")
	}
}

// waitProxy checks the port and, when it is free, starts the proxy and waits
// until it proves it holds this user's secret.
func waitProxy(ctx context.Context, cfg gatewayproxy.Config, secret []byte, start func() error) gatewayproxy.PortState {
	state := gatewayproxy.Check(ctx, cfg.Port, secret)
	if state != gatewayproxy.PortFree || start() != nil {
		return state
	}
	deadline := time.Now().Add(ensureStartWait)
	for state != gatewayproxy.PortOurs && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		state = gatewayproxy.Check(ctx, cfg.Port, secret)
	}
	return state
}

// fallBack points Claude Code at the gateway itself and says so once.
func fallBack(settings string, cfg gatewayproxy.Config, out io.Writer, why string) {
	changed, err := agentcfg.PointClaudeGateway(settings, cfg.URL(), cfg.Gateway, cfg.Gateway)
	if err != nil || !changed {
		return
	}
	msg := "CodeSynapse gateway proxy is not running (" + why + "), so Claude Code now talks to the gateway at " + cfg.Gateway +
		" directly, without failover. Restart this session to pick that up. Pick another port with `codesynapse mcp install --gateway --proxy-port N`."
	_ = json.NewEncoder(out).Encode(map[string]string{"systemMessage": msg})
}

// startProxy starts `codesynapse gateway proxy` detached from the hook, its
// output in the proxy's log file. It does not wait for it.
func startProxy() error {
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	files, err := gatewayproxy.DefaultFiles()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(files.Dir, 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(files.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()                           //nolint:errcheck // the child holds its own copy
	cmd := exec.Command(bin, "gateway", "proxy") //nolint:gosec,noctx // this binary, fixed arguments; the child outlives this command
	cmd.Stdout, cmd.Stderr = logf, logf
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func gatewayCmd(ec *app.Env) *cobra.Command {
	parent := &cobra.Command{Use: "gateway", Short: "Local failover proxy in front of the CodeSynapse agent gateway", GroupID: app.GroupBase}
	parent.AddCommand(app.NewSubCommand("proxy", &gatewayProxyOptions{EC: ec}, ec).
		WithShortDesc("Forward Claude Code's model requests to the gateway; while it is unreachable, send them straight to Anthropic").
		WithLongDesc("Listens on 127.0.0.1 only. Requests go to the gateway that `mcp install --gateway` saved. Only when the gateway cannot be reached (connection or TLS failure, or a 502/503/504 that is not the gateway's JSON) and nothing was streamed yet, a request goes to https://api.anthropic.com with your own Anthropic credential and without the CodeSynapse key. After 30 s the gateway is tried again. Direct requests are counted (time, number, model name) and reported to the gateway when it is back. Stops after --idle without requests; the Claude Code hook (--ensure) starts it again.").
		WithExample("  codesynapse gateway proxy --ensure   # what the Claude Code hook runs\n  codesynapse gateway proxy            # run in the foreground").
		WithNoArgs().
		Build())
	return parent
}
