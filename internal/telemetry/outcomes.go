package telemetry

import (
	"context"
	"slices"
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
	"not_eligible": true, "too_large": true, "has_thinking": true, "has_image": true,
	"excluded": true, "queue_timeout": true, "cancelled": true, "answer_timeout": true,
	"model_error": true, "bad_status": true, "empty_answer": true, "truncated": true,
	"bad_answer": true, "no_local_model": true, "bad_request": true, "system_not_text": true,
	"unsupported_block": true, "unsupported_tool": true, "count_tokens": true,
}

// GatewayReasons is the fixed set of fall-through reasons, sorted, for the
// statistics that list them.
func GatewayReasons() []string {
	out := make([]string, 0, len(gatewayReasons))
	for r := range gatewayReasons {
		out = append(out, r)
	}
	slices.Sort(out)
	return out
}

// GatewayTimeout says whether a reason is the local model being too slow:
// no queue slot in time, or no answer in time.
func GatewayTimeout(reason string) bool {
	return reason == "queue_timeout" || reason == "answer_timeout"
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

// RecordGatewayFallthrough counts a request the local model did not answer,
// with the route that took it instead. An empty reason means it answered or
// was never asked, and is not counted.
func RecordGatewayFallthrough(ctx context.Context, reason, route string) {
	if reason == "" {
		return
	}
	GatewayFallthrough.IncContext(ctx, GatewayReason(reason), GatewayRoute(route))
}

// GatewayTimings are the parts of one gateway request's time.
type GatewayTimings struct {
	// Asked says the request reached the local model's queue.
	Asked bool
	// Queue is the wait for the local model's slot; Answer the local model's
	// own time after it, which Answered says ended in an answer.
	Queue, Answer time.Duration
	Answered      bool
	// FirstByte is the time to the first byte of a streamed answer; zero
	// when the answer was not streamed or nothing was written.
	FirstByte time.Duration
	Route     string
}

// RecordGatewayTimings records the queue wait, the local model's time and the
// time to first byte of one request.
func RecordGatewayTimings(ctx context.Context, t GatewayTimings) {
	if t.Asked {
		GatewayQueueWait.ObserveContext(ctx, max(0, t.Queue.Seconds()))
		if t.Answer > 0 {
			outcome := "failed"
			if t.Answered {
				outcome = "answered"
			}
			GatewayLocalAnswer.ObserveContext(ctx, t.Answer.Seconds(), outcome)
		}
	}
	if t.FirstByte > 0 {
		if route := GatewayRoute(t.Route); route == "local" || route == "cloud" {
			GatewayFirstByte.ObserveContext(ctx, t.FirstByte.Seconds(), route)
		}
	}
}
