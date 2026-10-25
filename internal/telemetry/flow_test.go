package telemetry

import (
	"context"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestFlowStepsAreBounded(t *testing.T) {
	ctx := WithTenant(context.Background(), "org")
	RecordStepTokens(ctx, StepExtract, KindModelIn, 100)
	RecordStepTokens(ctx, StepExtract, KindModelIn, 0)             // no tokens: ignored
	RecordStepTokens(ctx, "/home/maria/secret.go", KindModelIn, 5) // unknown step: ignored
	RecordStepTokens(ctx, StepExtract, "prompt text", 5)           // unknown kind: ignored
	RecordStep(ctx, "repo-name", true)                             // unknown step: ignored
	RecordStep(ctx, StepExtract, false)

	out := text(t, Default)
	assert.Contains(t, out, `flow_step_tokens_total{`)
	assert.Contains(t, out, `step="offload.extract"`)
	assert.Contains(t, out, `kind="model_in"`)
	assert.Contains(t, out, `outcome="error"`)
	for _, leak := range []string{"secret.go", "repo-name", "prompt text"} {
		assert.False(t, strings.Contains(out, leak))
	}
}

func TestEstimateTokens(t *testing.T) {
	assert.Equal(t, EstimateTokens(0), 0)
	assert.Equal(t, EstimateTokens(1), 1)
	assert.Equal(t, EstimateTokens(400), 100)
}
