package llm

import (
	"context"
	"fmt"
	"time"

	"github.com/astrona-io/codesynapse/internal/modelpattern"
)

const (
	// DefaultGatewayWait is how long a gateway request waits for a free slot
	// on the local model when gateway_wait_seconds is not set.
	DefaultGatewayWait = 60 * time.Second
	maxGatewayWait     = 300
)

// GatewayTarget is the local model the agent gateway may answer from.
type GatewayTarget struct {
	BaseURL string
	Model   string
	APIKey  string
	// ContextWindow is the model's context in tokens (0 when not known); the
	// gateway sends requests that would not fit to the cloud.
	ContextWindow int
	// Exclude is the installation's normalized list of model patterns that
	// never go to this model (gateway_exclude).
	Exclude []string
	// Wait is how long a request waits for a free slot (gateway_wait_seconds).
	Wait time.Duration
	// Acquire takes the provider's queue slot, so gateway traffic and
	// CodeSynapse's own calls share one GPU in turn.
	Acquire func(ctx context.Context) (func(), error)
}

// validateGateway normalizes gateway_exclude and checks gateway_wait_seconds.
func validateGateway(p *ProviderConfig) error {
	list, err := modelpattern.Normalize(p.GatewayExclude)
	if err != nil {
		return fmt.Errorf("gateway_exclude: %w", err)
	}
	p.GatewayExclude = list
	// 0 is unset: the default.
	if p.GatewayWaitSeconds < 0 || p.GatewayWaitSeconds > maxGatewayWait {
		return fmt.Errorf("gateway_wait_seconds must be between 1 and %d", maxGatewayWait)
	}
	return nil
}

// GatewayTarget returns the first local provider that opted in with
// `gateway: true` and names a model.
func (r *Registry) GatewayTarget() (GatewayTarget, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.cfg.Providers {
		if !p.Gateway || !p.Local() || p.Model == "" || p.BaseURL == "" {
			continue
		}
		wait := DefaultGatewayWait
		if p.GatewayWaitSeconds > 0 {
			wait = time.Duration(p.GatewayWaitSeconds) * time.Second
		}
		queue := r.slots[p.Name]
		return GatewayTarget{
			BaseURL: p.BaseURL, Model: p.Model, APIKey: p.apiKey, ContextWindow: p.ContextWindow,
			Exclude: p.GatewayExclude, Wait: wait, Acquire: func(ctx context.Context) (func(), error) {
				return queue.acquire(WithTask(ctx, gatewayTask))
			},
		}, true
	}
	return GatewayTarget{}, false
}
