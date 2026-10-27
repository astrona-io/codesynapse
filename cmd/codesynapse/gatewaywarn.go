package main

import (
	"context"
	"path/filepath"
	"time"

	"github.com/astrona-io/codesynapse/internal/agentcfg"
)

// gatewayCheckTimeout keeps the session start from waiting on a dead gateway.
const gatewayCheckTimeout = 2 * time.Second

// gatewayWarning is a one-line notice for the person when Claude Code is
// pointed at the gateway (by `mcp install --gateway`) and the gateway does
// not answer. It is "" when nothing is configured or the gateway is fine. The
// key is read from the settings file and sent to the gateway only; it never
// reaches the warning or a log.
func gatewayWarning(ctx context.Context, home string) string {
	base, key, ok := agentcfg.ClaudeGatewayConfig(filepath.Join(home, ".claude", "settings.json"))
	if !ok {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, gatewayCheckTimeout)
	defer cancel()
	if err := agentcfg.CheckGateway(ctx, nil, base, key); err == nil {
		return ""
	}
	return "CodeSynapse gateway at " + base + " is not answering, so Claude Code requests will fail. Start it, or run `codesynapse mcp install --gateway --remove` to go direct."
}
