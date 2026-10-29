package telemetry

import (
	"context"
	"time"
)

// Outcomes of mcp_tool_calls_total. A refusal is a call the server turned
// down before the tool ran (permission, scope or policy); an error is one
// the tool ran and failed.
const (
	OutcomeOK      = "ok"
	OutcomeError   = "error"
	OutcomeRefused = "refused"
)

// Outcomes of llm_requests_total besides ok and error, mapped from status
// codes and context errors, never from error text.
const (
	OutcomeTimeout     = "timeout"
	OutcomeRateLimited = "rate_limited"
)

// Outcomes of mcp_local_answers_total: the server recorded the local answer
// (so the bridge gave it), or refused it (so the call went to the server).
const (
	LocalRecorded = "recorded"
	LocalRefused  = "refused"
)

var localOutcomes = map[string]bool{LocalRecorded: true, LocalRefused: true}

// RecordLocalAnswer counts a local answer report for tool. The tool label
// passes the AllowTools list; unknown outcomes are ignored.
func RecordLocalAnswer(ctx context.Context, tool, outcome string) {
	if !localOutcomes[outcome] {
		return
	}
	MCPLocalAnswers.IncContext(WithTool(ctx, tool), outcome)
}

// gatewayRoutes are the values of the gateway's Meta.Route.
var gatewayRoutes = map[string]bool{"local": true, "cloud": true, "rejected": true}

// gatewayReasons mirror the gateway's Fall* constants
// (services/api/internal/gateway/local.go); anything else is "other".
var gatewayReasons = map[string]bool{
	"excluded": true, "not_eligible": true, "too_large": true, "busy": true,
	"unavailable": true, "no_first_chunk": true, "bad_answer": true,
}

// GatewayRoute is the route label for route.
func GatewayRoute(route string) string {
	if gatewayRoutes[route] {
		return route
	}
	return "other"
}

// GatewayReason is the reason label for reason.
func GatewayReason(reason string) string {
	if gatewayReasons[reason] {
		return reason
	}
	return "other"
}

// RecordGatewayDuration records how long a gateway request took.
func RecordGatewayDuration(ctx context.Context, route string, d time.Duration) {
	GatewayDuration.ObserveContext(ctx, max(0, d.Seconds()), GatewayRoute(route))
}

// RecordGatewayFallthrough counts a request the local model passed to the
// cloud. An empty reason means it did not, and is not counted.
func RecordGatewayFallthrough(ctx context.Context, reason string) {
	if reason == "" {
		return
	}
	GatewayFallthrough.IncContext(ctx, GatewayReason(reason))
}
