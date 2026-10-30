package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProviderCheck is what a local provider's server says about the models
// config/llm.yaml names: whether it answers, whether the models are there
// and whether the context it really gives the chat model is as large as
// context_window. Only model names are sent; never a prompt or code.
type ProviderCheck struct {
	Name string `json:"name"`
	// Server is "ollama" when the server answers Ollama's own API, else
	// "openai" (any other OpenAI-compatible server).
	Server         string   `json:"server,omitempty"`
	Version        string   `json:"version,omitempty"`
	Reachable      bool     `json:"reachable"`
	Model          string   `json:"model"`
	ModelFound     bool     `json:"model_found"`
	EmbeddingModel string   `json:"embedding_model,omitempty"`
	EmbeddingFound bool     `json:"embedding_found,omitempty"`
	Tasks          []string `json:"tasks"`
	Gateway        bool     `json:"gateway"`
	// ContextWindow is the configured context_window. ModelContext is the
	// most the model was trained for, NumCtx a num_ctx its Modelfile sets
	// and LoadedContext what Ollama loaded it with; 0 when not known.
	ContextWindow int      `json:"context_window"`
	ModelContext  int      `json:"model_context,omitempty"`
	NumCtx        int      `json:"num_ctx,omitempty"`
	LoadedContext int      `json:"loaded_context,omitempty"`
	OK            bool     `json:"ok"`
	Problems      []string `json:"problems"`
	Warnings      []string `json:"warnings"`
	Seconds       float64  `json:"seconds"`
}

// LocalCheck is the check of every local provider, and what it means for
// the gateway.
type LocalCheck struct {
	Providers []ProviderCheck `json:"providers"`
	// Gateway names the provider the agent gateway answers from; "" when
	// no local provider has gateway: true and a model.
	Gateway  string   `json:"gateway"`
	OK       bool     `json:"ok"`
	Problems []string `json:"problems"`
	Warnings []string `json:"warnings"`
}

// checkTimeout bounds one provider's check; each request also has the
// discovery client's own timeout.
const checkTimeout = 20 * time.Second

// CheckLocal checks every local provider at once.
func (r *Registry) CheckLocal(ctx context.Context) LocalCheck {
	out := LocalCheck{Providers: []ProviderCheck{}, Problems: []string{}, Warnings: []string{}}
	if t, ok := r.GatewayTarget(); ok {
		out.Gateway = t.Name
	}
	var local []ProviderConfig
	for _, p := range r.Configs() {
		if p.Local() {
			local = append(local, p)
		}
	}
	out.Providers = make([]ProviderCheck, len(local))
	routes := r.taskRoutesByProvider()
	var wg sync.WaitGroup
	for i, p := range local {
		wg.Go(func() { out.Providers[i] = r.check(ctx, p, routes[p.Name], p.Name == out.Gateway) })
	}
	wg.Wait()
	switch {
	case len(local) == 0:
		out.Warnings = append(out.Warnings, "no local provider is configured: every task and gateway request goes to a cloud model")
	case out.Gateway == "":
		out.Warnings = append(out.Warnings, "no local provider has gateway: true (and a model): the gateway sends every request to the cloud")
	}
	for _, c := range out.Providers {
		if c.Gateway && !c.OK {
			out.Problems = append(out.Problems, fmt.Sprintf("the gateway's provider %s fails its check: requests fall through to the cloud or are cut short", c.Name))
		}
	}
	out.OK = len(out.Problems) == 0 && !slices.ContainsFunc(out.Providers, func(c ProviderCheck) bool { return !c.OK })
	return out
}

// Check checks provider name; ok is false when there is no such provider.
// A cloud provider is not probed.
func (r *Registry) Check(ctx context.Context, name string) (ProviderCheck, bool) {
	cfg, ok := r.Config(name)
	if !ok || !cfg.Local() {
		return ProviderCheck{}, false
	}
	t, _ := r.GatewayTarget()
	return r.check(ctx, cfg, r.taskRoutesByProvider()[name], t.Name == name), true
}

func (r *Registry) taskRoutesByProvider() map[string][]string {
	out := map[string][]string{}
	for task, name := range r.TaskRoutes() {
		out[name] = append(out[name], task)
	}
	for _, tasks := range out {
		slices.Sort(tasks)
	}
	return out
}

func (r *Registry) check(ctx context.Context, p ProviderConfig, tasks []string, gateway bool) (c ProviderCheck) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	c = ProviderCheck{
		Name: p.Name, Model: p.Model, EmbeddingModel: p.EmbeddingModel, ContextWindow: p.ContextWindow,
		Tasks: append([]string{}, tasks...), Gateway: gateway, Problems: []string{}, Warnings: []string{},
	}
	defer func() {
		c.OK = len(c.Problems) == 0
		c.Seconds = time.Since(start).Round(time.Millisecond).Seconds()
	}()
	// The model list goes through the provider's own client, like every
	// call CodeSynapse makes to it.
	served, err := r.Models(ctx, p.Name)
	if err != nil {
		c.problem("the server does not answer its model list: %s", shortReason(err))
		return c
	}
	c.Reachable = true
	has := func(m string) bool { return slices.Contains(served, m) || slices.Contains(served, m+":latest") }
	switch {
	case p.Model == Auto:
		c.problem("model: auto has not picked a chat model yet: the server offers none")
	case p.Model == "":
		c.warn("no model is set, so the server picks one and nothing here can be checked")
	case len(served) == 0:
		c.warn("the server lists no models, so %s cannot be checked", p.Model)
	case has(p.Model):
		c.ModelFound = true
	default:
		c.problem("the server does not have %s: pull it (ollama pull %s) or fix model in config/llm.yaml", p.Model, p.Model)
	}
	if p.EmbeddingModel != "" && p.EmbeddingModel != Auto && len(served) > 0 {
		c.EmbeddingFound = has(p.EmbeddingModel)
		if !c.EmbeddingFound {
			c.problem("the server does not have the embedding model %s: recall by meaning fails until it is pulled", p.EmbeddingModel)
		}
	}
	if slices.Contains(tasks, "embeddings") && (p.EmbeddingModel == "" || p.EmbeddingModel == Auto) {
		c.problem("embeddings are routed here but no embedding_model is set: recall by meaning is off")
	}
	if p.ContextWindow <= 0 {
		c.warn("context_window is not set, so prompts are not sized to the model")
	}
	root := strings.TrimSuffix(strings.TrimRight(p.BaseURL, "/"), "/v1")
	var version struct {
		Version string `json:"version"`
	}
	if getJSON(ctx, http.MethodGet, root+"/api/version", p.apiKey, nil, &version) != nil || version.Version == "" {
		c.Server = "openai"
		if p.ContextWindow > 0 {
			c.warn("the server does not say how much context it gives the model: make sure it serves at least %d tokens", p.ContextWindow)
		}
		return c
	}
	c.Server, c.Version = "ollama", version.Version
	c.checkOllama(ctx, root, p)
	return c
}

// checkOllama reads the chat model's context from Ollama's own API: the
// most it was trained for (/api/show), a num_ctx its Modelfile sets, and
// the context it is loaded with now (/api/ps). Ollama cuts longer prompts
// without an error, so a smaller real context is a problem.
func (c *ProviderCheck) checkOllama(ctx context.Context, root string, p ProviderConfig) {
	if c.ModelFound {
		show, err := ollamaShow(ctx, root, p)
		if err != nil {
			c.warn("Ollama does not describe %s: %s", p.Model, shortReason(err))
		} else {
			c.ModelContext, c.NumCtx = show.context(), show.numCtx()
			if len(show.Capabilities) > 0 && !slices.Contains(show.Capabilities, "completion") {
				c.problem("%s cannot chat (Ollama lists %s)", p.Model, strings.Join(show.Capabilities, ", "))
			}
		}
		c.LoadedContext = ollamaLoaded(ctx, root, p)
		c.compareContext(p.Model)
	}
	if c.EmbeddingFound {
		if show, err := ollamaShow(ctx, root, ProviderConfig{Model: p.EmbeddingModel, apiKey: p.apiKey}); err == nil &&
			len(show.Capabilities) > 0 && !slices.Contains(show.Capabilities, "embedding") {
			c.problem("%s is not an embedding model (Ollama lists %s)", p.EmbeddingModel, strings.Join(show.Capabilities, ", "))
		}
	}
}

func (c *ProviderCheck) compareContext(model string) {
	want := c.ContextWindow
	if want <= 0 {
		return
	}
	if c.ModelContext > 0 && want > c.ModelContext {
		c.problem("context_window is %d but %s takes at most %d tokens: lower context_window", want, model, c.ModelContext)
	}
	switch {
	case c.LoadedContext > 0 && c.LoadedContext < want:
		c.problem("Ollama loaded %s with %d tokens of context but context_window is %d: longer prompts are cut without an error. Start Ollama with OLLAMA_CONTEXT_LENGTH=%d (in the app: Settings, Context length) and reload the model",
			model, c.LoadedContext, want, want)
	case c.LoadedContext > 0:
	case c.NumCtx > 0 && c.NumCtx < want:
		c.problem("the Modelfile of %s sets num_ctx %d but context_window is %d: longer prompts are cut without an error", model, c.NumCtx, want)
	case c.NumCtx == 0:
		c.warn("%s is not loaded, so the context Ollama gives it is not known until the next request: make sure OLLAMA_CONTEXT_LENGTH (or the app's Context length) is at least %d, then check again", model, want)
	}
}

type ollamaModel struct {
	Capabilities []string       `json:"capabilities"`
	ModelInfo    map[string]any `json:"model_info"`
	Parameters   string         `json:"parameters"`
}

func ollamaShow(ctx context.Context, root string, p ProviderConfig) (ollamaModel, error) {
	var m ollamaModel
	err := getJSON(ctx, http.MethodPost, root+"/api/show", p.apiKey, map[string]string{"model": p.Model}, &m)
	return m, err
}

func (m ollamaModel) context() int {
	for k, v := range m.ModelInfo {
		if n, ok := v.(float64); ok && strings.HasSuffix(k, ".context_length") && n > 0 && n < 1<<31 {
			return int(n)
		}
	}
	return 0
}

var numCtxRe = regexp.MustCompile(`(?m)^num_ctx\s+(\d{1,10})\s*$`)

func (m ollamaModel) numCtx() int {
	if s := numCtxRe.FindStringSubmatch(m.Parameters); s != nil {
		n, _ := strconv.Atoi(s[1])
		return n
	}
	return 0
}

// ollamaLoaded is the context the running model was loaded with, 0 when it
// is not loaded or the server is too old to say.
func ollamaLoaded(ctx context.Context, root string, p ProviderConfig) int {
	var ps struct {
		Models []struct {
			Name          string `json:"name"`
			Model         string `json:"model"`
			ContextLength int    `json:"context_length"`
		} `json:"models"`
	}
	if getJSON(ctx, http.MethodGet, root+"/api/ps", p.apiKey, nil, &ps) != nil {
		return 0
	}
	for _, m := range ps.Models[:min(len(ps.Models), maxServed)] {
		for _, n := range []string{m.Name, m.Model} {
			if n == p.Model || n == p.Model+":latest" {
				return max(m.ContextLength, 0)
			}
		}
	}
	return 0
}

// shortReason keeps the cause of a failed request without the URL.
func shortReason(err error) string {
	var status httpStatus
	if errors.As(err, &status) {
		return status.Error()
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	s := err.Error()
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func (c *ProviderCheck) problem(format string, args ...any) {
	c.Problems = append(c.Problems, fmt.Sprintf(format, args...))
}

func (c *ProviderCheck) warn(format string, args ...any) {
	c.Warnings = append(c.Warnings, fmt.Sprintf(format, args...))
}
