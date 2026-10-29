package telemetry

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestPhaseFiveMetricsStayInTheirSets(t *testing.T) {
	AllowTools("git_status")
	ctx := WithTenant(context.Background(), "dddddddddddddddddddddddd")
	RecordLocalAnswer(ctx, "git_status", LocalRecorded)
	RecordLocalAnswer(ctx, "rm_rf", LocalRefused)
	RecordLocalAnswer(ctx, "git_status", "exploded")
	RecordGatewayDuration(ctx, "local", 300*time.Millisecond)
	RecordGatewayDuration(ctx, "elsewhere", time.Second)
	RecordGatewayFallthrough(ctx, "queue_timeout", "cloud")
	RecordGatewayFallthrough(ctx, "the model said no", "somewhere")
	RecordGatewayFallthrough(ctx, "", "cloud")
	RecordLocalAnswer(context.Background(), "git_status", LocalRecorded)

	out := text(t, Default)
	const id = `tenant_id="dddddddddddddddddddddddd"`
	assert.Contains(t, out, `mcp_local_answers_total{outcome="recorded",`+id+`,mcp_tool="git_status"`)
	assert.Contains(t, out, `mcp_local_answers_total{outcome="refused",`+id+`,mcp_tool="unknown"`)
	assert.False(t, strings.Contains(out, "rm_rf"), "tools pass the allowlist")
	assert.False(t, strings.Contains(out, "exploded"), "unknown outcomes are not recorded")
	assert.Contains(t, out, `gateway_request_duration_seconds_bucket{route="local",`+id)
	assert.Contains(t, out, `gateway_request_duration_seconds_count{route="other",`+id)
	assert.Contains(t, out, `gateway_fallthrough_total{reason="queue_timeout",route="cloud",`+id)
	assert.Contains(t, out, `gateway_fallthrough_total{reason="other",route="other",`+id)
	assert.False(t, strings.Contains(out, "somewhere"), "routes come from a fixed set")
	assert.False(t, strings.Contains(out, "model said no"), "reasons come from a fixed set")
	assert.Contains(t, out, `platform_mcp_local_answers_total{outcome="recorded",`)
}

func TestRequestHistogramsHaveATenantShare(t *testing.T) {
	for _, tc := range []struct {
		name  string
		h     *Histogram
		share int
	}{
		{"mcp_tool_duration_seconds", MCPToolDuration, toolShare},
		{"http_request_duration_seconds", HTTPDuration, routeShare},
		{"gateway_request_duration_seconds", GatewayDuration, gatewayShare},
		{"gateway_queue_wait_seconds", GatewayQueueWait, gatewayShare},
		{"gateway_local_answer_seconds", GatewayLocalAnswer, gatewayShare},
		{"gateway_first_byte_seconds", GatewayFirstByte, gatewayShare},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.h.share, tc.share)
			// Eight buckets plus sum, count and +Inf leave each tenant's share
			// well inside the budget, so many organisations fit.
			assert.True(t, tc.h.budget >= 5*tc.share, fmt.Sprintf("budget %d for share %d", tc.h.budget, tc.share))
		})
	}
	assert.Equal(t, len(MCPToolDuration.buckets), len(fast))
	assert.Equal(t, len(HTTPDuration.buckets), len(fast))
}

func TestFastBucketsCapAToolFloodPerTenant(t *testing.T) {
	r := NewRegistry()
	h := r.ScopedHistogram("tool_seconds", "Tool time.", fast, "tenant_id", "mcp_tool").TenantShare(3)
	tools := []string{"t0", "t1", "t2", "t3", "t4"}
	AllowTools(tools...)
	a := WithTenant(context.Background(), "eeeeeeeeeeeeeeeeeeeeeeee")
	b := WithTenant(context.Background(), "ffffffffffffffffffffffff")
	for _, tool := range tools {
		h.ObserveContext(WithTool(a, tool), 0.2, "", "")
	}
	h.ObserveContext(WithTool(b, "t4"), 0.2, "", "")
	assert.Equal(t, len(h.vals), 4, "the first tenant stops at three tools, the second still records")
	assert.Equal(t, r.dropped.vals["tool_seconds"], 2.0, "drops past the share are counted")
}

func TestGatewayTimingsGoToTheirHistograms(t *testing.T) {
	ctx := WithTenant(context.Background(), "abababababababababababab")
	const id = `tenant_id="abababababababababababab"`
	for name, tc := range map[string]struct {
		in   GatewayTimings
		want []string
		not  []string
	}{
		"local stream": {
			GatewayTimings{Asked: true, Queue: 2 * time.Second, Answer: 3 * time.Second, Answered: true, FirstByte: 4 * time.Second, Route: "local"},
			[]string{`gateway_queue_wait_seconds_count{` + id, `gateway_local_answer_seconds_count{outcome="answered",` + id, `gateway_first_byte_seconds_count{route="local",` + id},
			nil,
		},
		"queue timeout": {
			GatewayTimings{Asked: true, Queue: 60 * time.Second, Route: "cloud"},
			[]string{`gateway_queue_wait_seconds_bucket{` + id + `,mcp_tool="none",llm_provider="none",llm_model="none",le="60"}`},
			nil,
		},
		"local failure": {
			GatewayTimings{Asked: true, Answer: time.Second, Route: "cloud", FirstByte: time.Second},
			[]string{`gateway_local_answer_seconds_count{outcome="failed",` + id, `gateway_first_byte_seconds_count{route="cloud",` + id},
			nil,
		},
		"rejected has no first byte": {
			GatewayTimings{Route: "rejected", FirstByte: time.Second},
			nil,
			[]string{`gateway_first_byte_seconds_count{route="rejected"`},
		},
	} {
		t.Run(name, func(t *testing.T) {
			RecordGatewayTimings(ctx, tc.in)
			out := text(t, Default)
			for _, w := range tc.want {
				assert.Contains(t, out, w)
			}
			for _, n := range tc.not {
				assert.False(t, strings.Contains(out, n), n)
			}
		})
	}
}

func TestGatewayReasonsAreFixed(t *testing.T) {
	reasons := GatewayReasons()
	assert.Equal(t, len(reasons), len(gatewayReasons))
	for _, r := range reasons {
		assert.Equal(t, GatewayReason(r), r)
	}
	assert.Equal(t, GatewayReason("busy"), "other")
	assert.True(t, GatewayTimeout("queue_timeout") && GatewayTimeout("answer_timeout"), "both timeouts")
	assert.False(t, GatewayTimeout("model_error"), "an error is not a timeout")
}
