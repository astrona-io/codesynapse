package telemetry

import "context"

// Steps of the chat-memory flow (docs/operations/chat-memory-flow.md). The set
// is fixed so a step can be a label without growing series: an unknown step is
// dropped, never recorded.
const (
	StepRules        = "session_start.rules"    // ruleset added to a new chat
	StepRecallFacts  = "session_start.recall"   // facts added to a new chat
	StepOffloadQueue = "precompact.offload"     // older turns leaving the chat
	StepExtract      = "offload.extract"        // one local-model call over a batch of turns
	StepEmbedQuery   = "recall_context.embed"   // one local embedding of a question
	StepEmbedIndex   = "offload.embed_entities" // local embeddings of saved entity names
)

// Kinds of tokens a step moves. context_in is what enters the chat's context,
// context_out what leaves it; model_in and model_out are the local model's.
const (
	KindContextIn  = "context_in"
	KindContextOut = "context_out"
	KindModelIn    = "model_in"
	KindModelOut   = "model_out"
)

var flowSteps = map[string]bool{
	StepRules: true, StepRecallFacts: true, StepOffloadQueue: true,
	StepExtract: true, StepEmbedQuery: true, StepEmbedIndex: true,
}

var flowKinds = map[string]bool{KindContextIn: true, KindContextOut: true, KindModelIn: true, KindModelOut: true}

// The flow's instruments; record through RecordStep and RecordStepTokens so
// labels stay inside the fixed sets.
var (
	FlowStepCalls  = Default.ScopedCounter("flow_step_calls_total", "Chat-memory flow steps run, by step and outcome (ok, error).", "tenant_id", "step", "outcome")
	FlowStepTokens = Default.ScopedCounter("flow_step_tokens_total", "Tokens a chat-memory flow step moved: into or out of the chat context, or through the local model. Estimates unless the provider reported usage.", "tenant_id", "step", "kind")
)

// RecordStep counts one run of a flow step. Unknown steps and outcomes are
// ignored.
func RecordStep(ctx context.Context, step string, ok bool) {
	if !flowSteps[step] {
		return
	}
	outcome := "ok"
	if !ok {
		outcome = "error"
	}
	FlowStepCalls.IncContext(ctx, Tenant(ctx), step, outcome)
}

// RecordStepTokens adds tokens a step moved. Non-positive counts, unknown
// steps and unknown kinds are ignored.
func RecordStepTokens(ctx context.Context, step, kind string, tokens int) {
	if tokens <= 0 || !flowSteps[step] || !flowKinds[kind] {
		return
	}
	FlowStepTokens.AddContext(ctx, float64(tokens), Tenant(ctx), step, kind)
}

// EstimateTokens is the flow's size estimate for text: four bytes a token.
func EstimateTokens(bytes int) int { return (bytes + 3) / 4 }
