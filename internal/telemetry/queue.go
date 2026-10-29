package telemetry

import (
	"context"
	"time"
)

// Outcomes of llm_queue_wait_seconds.
const (
	QueueAcquired  = "acquired"
	QueueCancelled = "cancelled"
	QueueTimeout   = "timeout"
)

// Bases of llm_answer_start_seconds.
const (
	StartFirstToken = "first_token"
	StartFullAnswer = "full_answer"
)

var (
	queueOutcomes = map[string]bool{QueueAcquired: true, QueueCancelled: true, QueueTimeout: true}
	startBases    = map[string]bool{StartFirstToken: true, StartFullAnswer: true}
)

// RecordQueueWait records one wait for a provider's queue slot. ctx carries
// the provider through WithModel; unknown outcomes are ignored.
func RecordQueueWait(ctx context.Context, task, outcome string, d time.Duration) {
	if !queueOutcomes[outcome] {
		return
	}
	LLMQueueWait.ObserveContext(ctx, max(0, d.Seconds()), task, outcome)
}

// RecordAnswerStart records the time from the queue slot until the model
// started answering; unknown bases are ignored.
func RecordAnswerStart(ctx context.Context, task, basis string, d time.Duration) {
	if !startBases[basis] || d <= 0 {
		return
	}
	LLMAnswerStart.ObserveContext(ctx, d.Seconds(), task, basis)
}

// SetQueueLoad publishes the running and waiting requests of the tenant and
// provider in ctx together, so an export never sees one without the other.
func SetQueueLoad(ctx context.Context, running, waiting int) {
	Default.UpdateGauges(func() {
		LLMQueueRunning.SetContext(ctx, float64(running))
		LLMQueueWaiting.SetContext(ctx, float64(waiting))
	})
}

// SetQueueCapacity publishes a provider's slots. Only providers registered
// through AllowModels get a series, so the label stays configuration.
func SetQueueCapacity(ctx context.Context, provider string, slots int) {
	if !configuredProvider(provider) {
		return
	}
	LLMQueueCapacity.SetContext(ctx, float64(slots), provider)
}

func configuredProvider(provider string) bool {
	models.RLock()
	defer models.RUnlock()
	for id := range models.allowed {
		if id.provider == provider {
			return true
		}
	}
	return false
}
