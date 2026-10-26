package llm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// ErrNoServedModel means an auto provider's server offered no chat model at
// the last check.
var ErrNoServedModel = errors.New("the local server offers no chat model yet; pull one (ollama pull qwen3.8:27b) and it is picked up within minutes")

// Discovery is the last answer of an auto provider's server.
type Discovery struct {
	Models []ServedModel `json:"models"`
	Error  string        `json:"error,omitempty"`
	At     time.Time     `json:"at"`
}

// Auto reports whether name picks its models from its server.
func (p ProviderConfig) Auto() bool { return p.autoRaw != nil }

// Served returns the last discovery of auto provider name.
func (r *Registry) Served(name string) (Discovery, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.served[name]
	return d, ok
}

// Resolve asks every auto provider's server what it serves and picks the
// models to use. It returns one line per provider whose choice changed.
// A server that does not answer keeps its previous choice.
func (r *Registry) Resolve(ctx context.Context) []string {
	var changes []string
	for _, p := range r.Configs() {
		if p.autoRaw == nil {
			continue
		}
		served, err := Discover(ctx, p.BaseURL, p.apiKey)
		d := Discovery{Models: served, At: time.Now()}
		if err != nil {
			d.Error = err.Error()
		}
		r.mu.Lock()
		r.served[p.Name] = d
		r.mu.Unlock()
		if err != nil {
			continue
		}
		next, err := pick(p, served)
		if err != nil || (next.Model == p.Model && next.EmbeddingModel == p.EmbeddingModel && next.ContextWindow == p.ContextWindow) {
			continue
		}
		telemetry.AllowModels(next.Name, next.Model, next.EmbeddingModel)
		r.mu.Lock()
		for i := range r.cfg.Providers {
			if r.cfg.Providers[i].Name == next.Name {
				r.cfg.Providers[i] = next
			}
		}
		r.impl[next.Name] = newOpenAI(next)
		r.mu.Unlock()
		changes = append(changes, fmt.Sprintf("%s: model %s, embedding model %s, context %d", next.Name, orNone(next.Model), orNone(next.EmbeddingModel), next.ContextWindow))
	}
	return changes
}

// pick builds the configuration of auto provider p from what its server
// serves, starting again from the configuration as written.
func pick(p ProviderConfig, served []ServedModel) (ProviderConfig, error) {
	chat, embed := Best(served)
	next := *p.autoRaw
	next.autoRaw, next.apiKey = p.autoRaw, p.apiKey
	if p.autoRaw.Model == Auto && chat.Name != "" {
		next.Model = chat.Name
		if next.ContextWindow <= 0 && chat.Context > 0 {
			next.ContextWindow = chat.Context
		}
		if next.MaxOutputTokens <= 0 && next.ContextWindow >= 64_000 {
			// Thinking models spend part of the answer budget reasoning.
			next.MaxOutputTokens = 32_768
		}
	}
	if p.autoRaw.EmbeddingModel == Auto {
		next.EmbeddingModel = embed.Name
	}
	return effectiveBudgets(next)
}

func orNone(s string) string {
	if s == "" || s == Auto {
		return "none"
	}
	return s
}

// Add appends p and checks the configuration again.
func (c *Config) Add(p ProviderConfig, getenv func(string) string) error {
	c.Providers = append(c.Providers, p)
	return c.normalise(getenv)
}

// ResolveEvery resolves the auto providers every interval until ctx ends,
// reporting each change. The caller resolves once at start-up.
func (r *Registry) ResolveEvery(ctx context.Context, interval time.Duration, report func(change string)) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		rctx, cancel := context.WithTimeout(ctx, time.Minute)
		for _, c := range r.Resolve(rctx) {
			report(c)
		}
		cancel()
	}
}

// HasAuto reports whether any provider picks its models from its server.
func (r *Registry) HasAuto() bool {
	return slices.ContainsFunc(r.Configs(), ProviderConfig.Auto)
}

// DiscoverURLs are where a local model server usually listens, as the API
// container and a local process see them: Ollama, then LM Studio.
var DiscoverURLs = []string{
	"http://host.docker.internal:11434/v1", "http://host.containers.internal:11434/v1", "http://127.0.0.1:11434/v1",
	"http://host.docker.internal:1234/v1", "http://host.containers.internal:1234/v1", "http://127.0.0.1:1234/v1",
}

// DiscoverProvider looks for a local model server when nothing is
// configured: the first of urls that serves a chat model becomes the
// provider "local" with both models on auto.
func DiscoverProvider(ctx context.Context, urls []string) (ProviderConfig, bool) {
	for _, u := range urls {
		u = strings.TrimRight(strings.TrimSpace(u), "/")
		if u == "" || !(ProviderConfig{BaseURL: u}).Local() {
			continue
		}
		served, err := Discover(ctx, u, "")
		if err != nil || !slices.ContainsFunc(served, func(m ServedModel) bool { return m.Chat && !m.Embedding }) {
			continue
		}
		return ProviderConfig{Name: "local", Kind: KindOpenAI, BaseURL: u, Model: Auto, EmbeddingModel: Auto}, true
	}
	return ProviderConfig{}, false
}
