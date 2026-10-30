package main

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/astrona-io/codesynapse/internal/agentcfg"
	"github.com/astrona-io/codesynapse/internal/gatewayproxy"
)

// failoverInstaller sets up the failover proxy for a gateway: setUpFailover,
// or a fake in tests.
type failoverInstaller func(ctx context.Context, files gatewayproxy.Files, cfg gatewayproxy.Config, settings, key string) (string, error)

const gatewayInstallAction = "Run `codesynapse mcp install --gateway`, then start a new session"

// gatewayCheck looks at a gateway set up by `mcp install --gateway` and
// makes sure Claude Code reaches it through the failover proxy, so an outage
// of the gateway does not stop the chat. It never sets up a gateway nobody
// asked for and leaves a setup made with --failover=false alone. The fixes
// reuse the gateway URL and key already in settings.json.
func gatewayCheck(ctx context.Context, settings string, files gatewayproxy.Files, install failoverInstaller, start func() error) doctorCheck {
	c := doctorCheck{Name: "Gateway failover", OK: true}
	base, key, ours := agentcfg.ClaudeGatewayConfig(settings)
	if !ours {
		c.Detail = "no gateway set up (optional)"
		return c
	}
	base = strings.TrimRight(base, "/")
	reinstall := func(cfg gatewayproxy.Config) func(context.Context) error {
		return func(ctx context.Context) error {
			if err := agentcfg.CheckGateway(ctx, nil, cfg.Gateway, key); err != nil {
				return err
			}
			_, err := install(ctx, files, cfg, settings, key)
			return err
		}
	}
	cfg, err := files.Load()
	switch {
	case errors.Is(err, gatewayproxy.ErrNotConfigured) && files.DirectChosen():
		c.Detail = "Claude Code uses the gateway at " + base + " without failover (--failover=false)"
		return c
	case errors.Is(err, gatewayproxy.ErrNotConfigured):
		c.OK = false
		c.Action = gatewayInstallAction
		if base == (gatewayproxy.Config{Port: gatewayproxy.DefaultPort}).URL() || agentcfg.ClaudeGatewayHooks(settings) {
			// Points at a proxy whose config is gone: the gateway URL is lost.
			c.Detail = "Claude Code points at the failover proxy, but its config is missing"
			return c
		}
		c.Detail = "Claude Code uses the gateway at " + base + " directly: its requests fail while the gateway is down"
		c.fix = reinstall(gatewayproxy.Config{Gateway: base, Port: gatewayproxy.DefaultPort, KeyHash: gatewayproxy.HashKey(key)})
		return c
	case err != nil:
		c.OK = false
		c.Detail = "the failover proxy's config cannot be read: " + err.Error()
		c.Action = gatewayInstallAction
		return c
	}
	fixed := gatewayproxy.Config{Gateway: cfg.Gateway, Port: cfg.Port, KeyHash: gatewayproxy.HashKey(key)}
	switch {
	case cfg.KeyHash != fixed.KeyHash:
		c.OK = false
		c.Detail = "the failover proxy holds another key than Claude Code sends"
		c.Action = gatewayInstallAction
		c.fix = reinstall(fixed)
	case !agentcfg.ClaudeGatewayHooks(settings):
		c.OK = false
		c.Detail = "the hooks that start the failover proxy are missing from " + settings
		c.Action = gatewayInstallAction
		c.fix = reinstall(fixed)
	case base == cfg.URL():
		secret, err := files.Secret()
		if err != nil {
			c.OK = false
			c.Detail = "the failover proxy's secret cannot be read"
			c.Action = gatewayInstallAction
			c.fix = reinstall(fixed)
			return c
		}
		switch gatewayproxy.Check(ctx, cfg.Port, secret) {
		case gatewayproxy.PortOurs:
			c.Detail = "the proxy on " + cfg.URL() + " runs and fails over from " + cfg.Gateway
		case gatewayproxy.PortFree:
			c.Detail = "the proxy on " + cfg.URL() + " is idle; the next Claude Code session starts it"
		default:
			c.OK = false
			c.Detail = "another program holds port " + strconv.Itoa(cfg.Port)
			c.Action = "Pick another port with `codesynapse mcp install --gateway --proxy-port N`, then start a new session"
		}
	case base == cfg.Gateway:
		c.OK = false
		c.Detail = "the proxy did not start, so Claude Code fell back to the gateway at " + cfg.Gateway + " without failover"
		c.Action = "Start a new Claude Code session; if it falls back again, see " + files.LogPath()
		c.fix = func(ctx context.Context) error {
			ensureProxy(ctx, files, cfg, settings, start, io.Discard)
			if now, _, _ := agentcfg.ClaudeGatewayConfig(settings); strings.TrimRight(now, "/") != cfg.URL() {
				return errors.New("the proxy did not start (log: " + files.LogPath() + ")")
			}
			return nil
		}
	default:
		c.OK = false
		c.Detail = "Claude Code uses " + base + ", not the failover proxy for " + cfg.Gateway
		c.Action = gatewayInstallAction
		c.fix = reinstall(fixed)
	}
	return c
}
