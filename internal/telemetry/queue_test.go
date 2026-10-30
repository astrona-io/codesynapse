package telemetry

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestQueueMetricsStayInTheirSets(t *testing.T) {
	AllowModels("queue-local", "qwen")
	const id = `tenant_id="cdcdcdcdcdcdcdcdcdcdcdcd"`
	ctx := WithModel(WithTenant(context.Background(), "cdcdcdcdcdcdcdcdcdcdcdcd"), "queue-local", "qwen")
	RecordQueueWait(ctx, "review", QueueAcquired, 0)
	RecordQueueWait(ctx, "review", QueueTimeout, 30*time.Second)
	RecordQueueWait(ctx, "review", "exploded", time.Second)
	RecordAnswerStart(ctx, "review", StartFirstToken, 2*time.Second)
	RecordAnswerStart(ctx, "review", "guess", 2*time.Second)
	SetQueueLoad(ctx, 1, 3)
	SetQueueLoad(WithModel(context.Background(), "queue-local", "qwen"), 1, 0)
	SetQueueCapacity(context.Background(), "queue-local", 2)
	SetQueueCapacity(context.Background(), "never-configured", 9)

	out := text(t, Default)
	model := `llm_provider="queue-local",llm_model="qwen"`
	assert.Contains(t, out, `llm_queue_wait_seconds_bucket{task="review",outcome="acquired",`+id+`,mcp_tool="none",`+model+`,le="0.005"} 1`)
	assert.Contains(t, out, `llm_queue_wait_seconds_count{task="review",outcome="timeout",`+id)
	assert.False(t, strings.Contains(out, "exploded"), "outcomes are a fixed set")
	assert.Contains(t, out, `llm_answer_start_seconds_count{task="review",basis="first_token",`+id)
	assert.False(t, strings.Contains(out, `basis="guess"`), "bases are a fixed set")
	assert.Contains(t, out, `llm_queue_running{`+id+`,mcp_tool="none",`+model+`} 1`)
	assert.Contains(t, out, `llm_queue_waiting{`+id+`,mcp_tool="none",`+model+`} 3`)
	assert.Contains(t, out, `platform_llm_queue_running{mcp_tool="none",`+model+`} 1`)
	assert.Contains(t, out, `platform_llm_queue_capacity{llm_provider="queue-local"} 2`)
	assert.False(t, strings.Contains(out, "never-configured"), "capacity names configured providers only")
	assert.Equal(t, LLMQueueWait.share, llmQueueShare)
	assert.True(t, LLMQueueWait.budget >= 5*llmQueueShare, "many organisations fit")
}
