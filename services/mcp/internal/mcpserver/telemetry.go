package mcpserver

import (
	"context"
	"encoding/json"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// telemetryMiddleware records sessions, tool and prompt calls, latency and
// response size for VictoriaMetrics. Only names of tools and prompts the
// server accepted become labels: unknown names fail at the protocol level
// (err != nil) and are counted as "unknown", so a client cannot mint series.
// A call the authorization middleware turned down (permission, scope or
// policy, the calls it audits as denied) is counted as refused, not error.
func telemetryMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case "initialize", "server/discover":
				res, err := next(ctx, method, req)
				if err == nil {
					telemetry.MCPSessions.IncContext(ctx, telemetry.Tenant(ctx))
				}
				return res, err
			case "tools/call", "prompts/get":
				start := time.Now()
				refused := new(bool)
				res, err := next(context.WithValue(ctx, refusalKey{}, refused), method, req)
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
				ctx = telemetry.WithTool(ctx, name)
				tool, tenant := telemetry.Tool(ctx), telemetry.Tenant(ctx)
				telemetry.MCPToolCalls.IncContext(ctx, tenant, tool, callOutcome(res, err, *refused))
				telemetry.MCPToolDuration.ObserveContext(ctx, time.Since(start).Seconds(), tenant, tool)
				if res != nil {
					if b, merr := json.Marshal(res); merr == nil {
						telemetry.MCPResponseTokens.AddContext(ctx, float64(len(b)/4), tenant, tool)
					}
				}
				return res, err
			}
			return next(ctx, method, req)
		}
	}
}

// refusalKey carries a *bool the authorization middleware sets when it turns
// a call down, so the telemetry middleware around it can tell a refusal from
// a tool that ran and failed.
type refusalKey struct{}

// markRefused notes that the server refused the call in ctx.
func markRefused(ctx context.Context) {
	if p, ok := ctx.Value(refusalKey{}).(*bool); ok {
		*p = true
	}
}

// callOutcome is the outcome label of mcp_tool_calls_total.
func callOutcome(res mcp.Result, err error, refused bool) string {
	r, ok := res.(*mcp.CallToolResult)
	switch {
	case refused:
		return telemetry.OutcomeRefused
	case err != nil || (ok && r != nil && r.IsError):
		return telemetry.OutcomeError
	}
	return telemetry.OutcomeOK
}
