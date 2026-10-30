package llm

import (
	"context"
	"fmt"
	"time"

	"github.com/astrona-io/codesynapse/internal/modelpattern"
)

const (
	// DefaultGatewayWait is how long a gateway request waits for a free slot
	// on the local model when gateway_wait_seconds is not set. Measured on a
	// 27B model on a Mac (docs/operations/gateway.md, Local model
	// performance), a 20k-token prompt holds the slot for about a minute, so
	// a request queued behind one is better off in the cloud after half that.
	DefaultGatewayWait = 30 * time.Second
	maxGatewayWait     = 300
	// DefaultGatewayFirstToken is how long the local model may take to send
	// its first token (prompt prefill) once the request has the slot, when
	// gateway_first_token_seconds is not set. Measured: about 12 s for 5k
	// tokens, 56 s for 20k and three minutes for 40k; 90 s keeps what starts
	// within a minute and stops paying for prompts that will not.
	DefaultGatewayFirstToken = 90 * time.Second
	maxGatewayFirstToken     = 600
)

// GatewayTarget is the local model the agent gateway may answer from.
type GatewayTarget struct {
	BaseURL string
	Model   string
	APIKey  string
	// ContextWindow is the model's context in tokens (0 when not known); the
	// gateway sends requests that would not fit to the cloud.
	ContextWindow int
	// MaxOutput is the provider's effective max_output_tokens: the least
	// answer room the gateway keeps when it lowers a request's max_tokens.
	MaxOutput int
	// Exclude is the installation's normalized list of model patterns that
	// never go to this model (gateway_exclude).
	Exclude []string
	// Wait is how long a request waits for a free slot (gateway_wait_seconds).
	Wait time.Duration
	// FirstToken bounds the time to the model's first token
	// (gateway_first_token_seconds) and Timeout the whole answer
	// (timeout_seconds). Either running out before anything was written
	// sends the request to the cloud.
	FirstToken, Timeout time.Duration
	// Options are the provider's prompt cache fields (keep_alive on Ollama,
	// cache_prompt on llama.cpp), so the model and the cached prefix of the
	// agent's previous turn stay loaded between turns.
	Options map[string]any
	// Acquire takes the provider's queue slot, so gateway traffic and
	// CodeSynapse's own calls share one GPU in turn.
	Acquire func(ctx context.Context) (func(), error)
	// AcquireFirst takes the same queue's slot ahead of ordinary requests,
	// for the gateway's quick requests; each waiting request is overtaken a
	// bounded number of times (maxPassed).
	AcquireFirst func(ctx context.Context) (func(), error)
	// Effort and DisableThinking are the provider's reasoning settings, so
	// gateway requests think as little as the provider's own calls do.
	Effort          string
	DisableThinking bool
}

// validateGateway normalizes gateway_exclude and checks gateway_wait_seconds
// and gateway_first_token_seconds.
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
	if p.GatewayFirstTokenSeconds < 0 || p.GatewayFirstTokenSeconds > maxGatewayFirstToken {
		return fmt.Errorf("gateway_first_token_seconds must be between 1 and %d", maxGatewayFirstToken)
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
		first := DefaultGatewayFirstToken
		if p.GatewayFirstTokenSeconds > 0 {
			first = time.Duration(p.GatewayFirstTokenSeconds) * time.Second
		}
		queue := r.slots[p.Name]
		return GatewayTarget{
			BaseURL: p.BaseURL, Model: p.Model, APIKey: p.apiKey, ContextWindow: p.ContextWindow, MaxOutput: p.MaxOutputTokens,
			Exclude: p.GatewayExclude, Wait: wait, FirstToken: first, Timeout: time.Duration(p.TimeoutSeconds) * time.Second,
			Options: kvCacheOptions(p), Effort: p.Effort, DisableThinking: p.DisableThinking, Acquire: func(ctx context.Context) (func(), error) {
				return queue.acquire(WithTask(ctx, gatewayTask))
			},
			AcquireFirst: func(ctx context.Context) (func(), error) {
				return queue.acquireFirst(WithTask(ctx, gatewayTask))
			},
		}, true
	}
	return GatewayTarget{}, false
}
