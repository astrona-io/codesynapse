package main

import (
	"context"
	"path/filepath"
	"time"

	"github.com/astrona-io/codesynapse/internal/agentcfg"
	"github.com/astrona-io/codesynapse/internal/gatewayproxy"
)

// gatewayCheckTimeout keeps the session start from waiting on a dead gateway.
const gatewayCheckTimeout = 2 * time.Second

// gatewayWarning is a one-line notice for the person when Claude Code is
// pointed at the gateway (by `mcp install --gateway`) and the gateway does
// not answer. It is "" when nothing is configured or the gateway is fine. The
// key is read from the settings file and sent to the gateway only; it never
// reaches the warning or a log.
func gatewayWarning(ctx context.Context, home string) string {
	files, err := gatewayproxy.DefaultFiles()
	if err != nil {
		files = gatewayproxy.Files{}
	}
	return gatewayWarningWith(ctx, home, files)
}

func gatewayWarningWith(ctx context.Context, home string, files gatewayproxy.Files) string {
	base, key, ok := agentcfg.ClaudeGatewayConfig(filepath.Join(home, ".claude", "settings.json"))
	if !ok {
		return ""
	}
	proxied := false
	if files.Dir != "" {
		if cfg, err := files.Load(); err == nil && base == cfg.URL() {
			// Claude Code talks to the failover proxy; what matters is the
			// gateway behind it.
			base, proxied = cfg.Gateway, true
		}
	}
	ctx, cancel := context.WithTimeout(ctx, gatewayCheckTimeout)
	defer cancel()
	if err := agentcfg.CheckGateway(ctx, nil, base, key); err == nil {
		return ""
	}
	if proxied {
		return "CodeSynapse gateway at " + base + " is not answering. While it is down, the local failover proxy sends Claude Code requests straight to Anthropic with your own credential, without local routing or the server's audit; they are counted and reported when the gateway is back."
	}
	return "CodeSynapse gateway at " + base + " is not answering, so Claude Code requests will fail. Start it, or run `codesynapse mcp install --gateway --remove` to go direct."
}
