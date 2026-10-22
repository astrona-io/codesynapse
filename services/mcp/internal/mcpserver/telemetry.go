package mcpserver

import (
	"context"
	"encoding/json"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/parisnakitakejser/codesynapse/internal/telemetry"
)

// telemetryMiddleware records sessions, tool and prompt calls, latency and
// response size for VictoriaMetrics. Only names of tools and prompts the
// server accepted become labels: unknown names fail at the protocol level
// (err != nil) and are counted as "unknown", so a client cannot mint series.
func telemetryMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case "initialize", "server/discover":
				res, err := next(ctx, method, req)
				if err == nil {
					telemetry.MCPSessions.Inc(telemetry.Tenant(ctx))
				}
				return res, err
			case "tools/call", "prompts/get":
				start := time.Now()
				res, err := next(ctx, method, req)
				name := ""
				switch p := req.GetParams().(type) {
				case *mcp.CallToolParamsRaw:
					name = p.Name
				case *mcp.GetPromptParams:
					name = "prompt:" + p.Name
				}
				if err == nil {
					telemetry.AllowTools(name)
				}
				tool, tenant := telemetry.ToolLabel(name), telemetry.Tenant(ctx)
				outcome := "ok"
				if r, ok := res.(*mcp.CallToolResult); err != nil || (ok && r != nil && r.IsError) {
					outcome = "error"
				}
				telemetry.MCPToolCalls.Inc(tenant, tool, outcome)
				telemetry.MCPToolDuration.Observe(time.Since(start).Seconds(), tenant, tool)
				if res != nil {
					if b, merr := json.Marshal(res); merr == nil {
						telemetry.MCPResponseTokens.Add(float64(len(b)/4), tenant, tool)
					}
				}
				return res, err
			}
			return next(ctx, method, req)
		}
	}
}
