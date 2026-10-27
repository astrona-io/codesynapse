package llm

import "context"

// GatewayTarget is the local model the agent gateway may answer from.
type GatewayTarget struct {
	BaseURL string
	Model   string
	APIKey  string
	// Acquire takes the provider's queue slot, so gateway traffic and
	// CodeSynapse's own calls share one GPU in turn.
	Acquire func(ctx context.Context) (func(), error)
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
		return GatewayTarget{BaseURL: p.BaseURL, Model: p.Model, APIKey: p.apiKey, Acquire: r.slots[p.Name].acquire}, true
	}
	return GatewayTarget{}, false
}
