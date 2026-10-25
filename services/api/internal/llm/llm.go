// Package llm talks to language models CodeSynapse uses for generated
// reports: local servers (MLX, Ollama, vLLM, LM Studio) and cloud providers
// through the OpenAI-compatible chat API, and Anthropic natively.
//
// Providers are configured by the operator (config file or environment),
// never by API callers, and each task is routed to one provider so cheap
// local models do routine work. Everything sent is built from the graph
// (summaries, signatures, findings) and secret-redacted; source snippets
// go only to providers that allow them. Model output is untrusted text.
package llm

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"gopkg.in/yaml.v3"

	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/redact"
	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// Kinds of provider.
const (
	KindOpenAI    = "openai"    // any OpenAI-compatible /chat/completions server
	KindAnthropic = "anthropic" // Claude via the Anthropic SDK
)

// Tasks a provider can be routed to.
var Tasks = []string{"security-triage", "review", "briefing", "fix", "memory", "embeddings", "code-annotation", "pr-summary", "cleanup-briefing"}

// LocalTasks run only on a local model: they read a diff or the cleanup plan,
// which name paths and symbols of a private change. Without a routed local
// provider they pick the first local one; with none, the task is unavailable.
var LocalTasks = []string{"pr-summary", "cleanup-briefing"}

// ErrNoLocalModel means a local-only task has no local provider to run on.
var ErrNoLocalModel = errors.New("this task runs only on a local model and none is configured")

// Request is one completion request.
type Request struct {
	System    string
	User      string
	MaxTokens int
}

// Response is a completion.
type Response struct {
	Text  string
	Model string
	// Preflight is admission accounting, separate from reported usage and cost.
	Preflight InputCount
	// InputTokens counts the whole prompt; CachedInputTokens is the part
	// the provider served from its prompt cache.
	InputTokens           int
	CachedInputTokens     int
	CacheWriteInputTokens int
	// UsageKnown requires final input and output usage from the provider.
	UsageKnown    bool
	UsageReported bool
	OutputTokens  int
	Duration      time.Duration
	// FirstToken is the time to the first streamed token (0 when the
	// provider does not stream).
	FirstToken time.Duration
}

type taskKey struct{}

// WithTask labels LLM calls made with ctx by task (one of Tasks) for the
// metrics.
func WithTask(ctx context.Context, task string) context.Context {
	return context.WithValue(ctx, taskKey{}, task)
}

func taskOf(ctx context.Context) string {
	if t, ok := ctx.Value(taskKey{}).(string); ok && slices.Contains(Tasks, t) {
		return t
	}
	return "other"
}

// Provider completes prompts.
type Provider interface {
	Complete(ctx context.Context, req Request) (Response, error)
	// Models lists model IDs the server offers (health check).
	Models(ctx context.Context) ([]string, error)
}

// contextMargin reserves space for provider framing the estimate cannot verify.
const contextMargin = 1024

// budgets fills in token budgets and the timeout. Without a context window
// the defaults are conservative, because CodeSynapse cannot know what an
// unknown server accepts.
func budgets(p *ProviderConfig) {
	if p.InputBudgetPolicy == "" {
		p.InputBudgetPolicy = "estimate"
	}
	if p.MaxOutputTokens <= 0 {
		p.MaxOutputTokens = 8192
	}
	if p.ContextWindow > 0 {
		p.MaxOutputTokens = min(p.MaxOutputTokens, p.ContextWindow/2)
		room := p.ContextWindow - p.MaxOutputTokens - contextMargin
		if p.MaxInputTokens <= 0 || p.MaxInputTokens > room {
			p.MaxInputTokens = room
		}
	}
	if p.MaxInputTokens <= 0 {
		p.MaxInputTokens = 32_000
	}
	if p.TimeoutSeconds <= 0 {
		// Prefill on local hardware runs at hundreds of tokens a second, so
		// large prompts need minutes.
		p.TimeoutSeconds = min(86400, max(300, p.MaxInputTokens/100))
	}
}

// ProviderConfig configures one provider.
type ProviderConfig struct {
	Name    string `yaml:"name" json:"name"`
	Kind    string `yaml:"kind" json:"kind"`
	BaseURL string `yaml:"base_url" json:"base_url"`
	Model   string `yaml:"model" json:"model"`
	// APIKeyEnv names the environment variable holding the key; empty for
	// local servers without authentication.
	APIKeyEnv string `yaml:"api_key_env" json:"api_key_env,omitempty"`
	// MaxOutputTokens caps each answer; reasoning models need headroom.
	MaxOutputTokens int `yaml:"max_output_tokens" json:"max_output_tokens"`
	// MaxInputTokens caps the context CodeSynapse packs into a prompt.
	// Defaults to the context window minus the output budget.
	MaxInputTokens int `yaml:"max_input_tokens" json:"max_input_tokens"`
	// InputBudgetPolicy allows labelled estimates or requires a verified exact count.
	InputBudgetPolicy string `yaml:"input_budget_policy" json:"input_budget_policy,omitempty"`
	// BytesPerToken is how many request bytes the estimate counts as one
	// input token. The default 1 never undercounts; a local model whose
	// tokenizer reads about three or four bytes a token can set 3 and get
	// about three times more into each request, with fewer calls.
	BytesPerToken int `yaml:"bytes_per_token" json:"bytes_per_token,omitempty"`
	// ContextWindow is the model's total context (prompt + answer). Input
	// and output budgets are kept inside it.
	ContextWindow  int `yaml:"context_window" json:"context_window"`
	TimeoutSeconds int `yaml:"timeout_seconds" json:"timeout_seconds"`
	// SendSource allows short source snippets in prompts. Defaults to true
	// for local providers and false for remote ones.
	SendSource *bool `yaml:"send_source" json:"-"`
	// Effort is the Anthropic output effort (low, medium, high, xhigh, max).
	Effort string `yaml:"effort" json:"effort,omitempty"`
	// DisableThinking turns off the reasoning phase of local reasoning
	// models (Qwen3, DeepSeek-R1 style) for faster structured answers. Sent
	// as chat_template_kwargs.enable_thinking=false (MLX, vLLM, SGLang).
	DisableThinking bool `yaml:"disable_thinking" json:"disable_thinking,omitempty"`
	// EmbeddingModel names the model the provider's /embeddings endpoint
	// uses (e.g. nomic-embed-text on Ollama); empty means no embeddings.
	EmbeddingModel string `yaml:"embedding_model" json:"embedding_model,omitempty"`
	// Prices in US dollars per million tokens, for the cost estimate in the
	// statistics. Missing remote rates leave cost unknown. PricingConfigured
	// explicitly supplies all rates, including intentionally zero rates.
	PriceInputPerMTok       float64 `yaml:"price_input_per_mtok" json:"price_input_per_mtok,omitempty"`
	PriceCachedInputPerMTok float64 `yaml:"price_cached_input_per_mtok" json:"price_cached_input_per_mtok,omitempty"`
	PricingConfigured       bool    `yaml:"pricing_configured" json:"pricing_configured,omitempty"`
	PriceCacheWritePerMTok  float64 `yaml:"price_cache_write_per_mtok" json:"price_cache_write_per_mtok,omitempty"`
	PriceOutputPerMTok      float64 `yaml:"price_output_per_mtok" json:"price_output_per_mtok,omitempty"`

	apiKey string
}

// Config is the whole LLM configuration.
type Config struct {
	Providers []ProviderConfig `yaml:"providers"`
	// Tasks maps a task to a provider name; Default covers the rest.
	Tasks   map[string]string `yaml:"tasks"`
	Default string            `yaml:"default"`
}

// Local reports whether the provider runs on this machine or network,
// judged by its base URL. Remote providers get no source by default.
func (p ProviderConfig) Local() bool {
	if p.Kind == KindAnthropic && p.BaseURL == "" {
		return false
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || host == "host.docker.internal" || host == "host.containers.internal" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".lan") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && (addr.IsLoopback() || addr.IsPrivate())
}

// AllowsSource reports whether source snippets may be sent.
func (p ProviderConfig) AllowsSource() bool {
	if p.SendSource != nil {
		return *p.SendSource
	}
	return p.Local()
}

// HasKey reports whether an API key is configured (never the key itself).
func (p ProviderConfig) HasKey() bool { return p.apiKey != "" }

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

func (c *Config) normalise(getenv func(string) string) error {
	seen := map[string]bool{}
	for i := range c.Providers {
		p := &c.Providers[i]
		if err := validateBudgetConfig(*p); err != nil {
			return fmt.Errorf("provider %q: %w", p.Name, err)
		}
		if !nameRe.MatchString(p.Name) {
			return fmt.Errorf("provider name %q must match %s", p.Name, nameRe)
		}
		if seen[p.Name] {
			return fmt.Errorf("duplicate provider %q", p.Name)
		}
		seen[p.Name] = true
		switch p.Kind {
		case "", KindOpenAI:
			p.Kind = KindOpenAI
			if p.BaseURL == "" {
				return fmt.Errorf("provider %q needs base_url", p.Name)
			}
		case KindAnthropic:
			if p.Model == "" {
				p.Model = "claude-opus-5-5"
			}
			if p.ContextWindow <= 0 {
				p.ContextWindow = 200_000
			}
			if p.MaxOutputTokens <= 0 {
				p.MaxOutputTokens = 32_000
			}
			if p.APIKeyEnv == "" {
				p.APIKeyEnv = "ANTHROPIC_API_KEY"
			}
		default:
			return fmt.Errorf("provider %q: kind must be %s or %s", p.Name, KindOpenAI, KindAnthropic)
		}
		if p.BaseURL != "" && !strings.HasPrefix(p.BaseURL, "http://") && !strings.HasPrefix(p.BaseURL, "https://") {
			return fmt.Errorf("provider %q: base_url must be http(s)", p.Name)
		}
		for _, price := range []float64{p.PriceInputPerMTok, p.PriceCachedInputPerMTok, p.PriceCacheWritePerMTok, p.PriceOutputPerMTok} {
			if price < 0 || math.IsNaN(price) || math.IsInf(price, 0) {
				return fmt.Errorf("provider %q: token prices must be finite and nonnegative", p.Name)
			}
		}
		p.BaseURL = strings.TrimRight(p.BaseURL, "/")
		normalized, err := effectiveBudgets(*p)
		if err != nil {
			return fmt.Errorf("provider %q: %w", p.Name, err)
		}
		*p = normalized
		if p.APIKeyEnv != "" {
			p.apiKey = getenv(p.APIKeyEnv)
		}
	}
	if c.Default == "" && len(c.Providers) > 0 {
		c.Default = c.Providers[0].Name
	}
	for task, name := range c.Tasks {
		if !slices.Contains(Tasks, task) {
			return fmt.Errorf("unknown task %q (tasks: %v)", task, Tasks)
		}
		if !seen[name] {
			return fmt.Errorf("task %q routes to unknown provider %q", task, name)
		}
	}
	if c.Default != "" && !seen[c.Default] {
		return fmt.Errorf("default provider %q is not configured", c.Default)
	}
	return nil
}

// Load reads the YAML file at path (optional) and adds a provider from the
// quick-setup environment variables CODESYNAPSE_LLM_BASE_URL,
// CODESYNAPSE_LLM_MODEL and CODESYNAPSE_LLM_API_KEY_ENV when set.
func Load(path string, getenv func(string) string) (Config, error) {
	var c Config
	if path != "" {
		data, err := os.ReadFile(path) //nolint:gosec // operator-provided config path
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return c, err
		default:
			dec := yaml.NewDecoder(strings.NewReader(string(data)))
			dec.KnownFields(true)
			if err := dec.Decode(&c); err != nil {
				return c, fmt.Errorf("%s: %w", path, err)
			}
		}
	}
	if base := getenv("CODESYNAPSE_LLM_BASE_URL"); base != "" {
		c.Providers = append(c.Providers, ProviderConfig{
			Name: "env", Kind: KindOpenAI, BaseURL: base, Model: getenv("CODESYNAPSE_LLM_MODEL"),
			APIKeyEnv: getenv("CODESYNAPSE_LLM_API_KEY_ENV"),
		})
	}
	return c, c.normalise(getenv)
}

// Usage counts what a provider has done since start.
type Usage struct {
	Requests     int     `json:"requests"`
	Failures     int     `json:"failures"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	AvgSeconds   float64 `json:"avg_seconds"`
	LastError    string  `json:"last_error,omitempty"`
}

// Registry holds the configured providers.
type Registry struct {
	cfg     Config
	impl    map[string]Provider
	mu      sync.Mutex
	usage   map[string]*Usage
	counter promptCounter
}

// NewRegistry builds providers from cfg.
func NewRegistry(cfg Config) *Registry {
	// Providers and preflight must retain the same configuration after startup.
	cfg.Providers = slices.Clone(cfg.Providers)
	for i := range cfg.Providers {
		if normalized, err := effectiveBudgets(cfg.Providers[i]); err == nil {
			cfg.Providers[i] = normalized
		}
		if cfg.Providers[i].SendSource != nil {
			allowed := *cfg.Providers[i].SendSource
			cfg.Providers[i].SendSource = &allowed
		}
	}
	cfg.Tasks = maps.Clone(cfg.Tasks)
	r := &Registry{cfg: cfg, impl: map[string]Provider{}, usage: map[string]*Usage{}, counter: requestByteCounter{}}
	for _, p := range cfg.Providers {
		telemetry.AllowModels(p.Name, p.Model, p.EmbeddingModel)
		switch p.Kind {
		case KindAnthropic:
			r.impl[p.Name] = newAnthropic(p)
		default:
			r.impl[p.Name] = newOpenAI(p)
		}
		r.usage[p.Name] = &Usage{}
	}
	return r
}

// Configs returns provider configurations (no secrets).
func (r *Registry) Configs() []ProviderConfig { return slices.Clone(r.cfg.Providers) }

// Config returns the configuration of name.
func (r *Registry) Config(name string) (ProviderConfig, bool) {
	for _, p := range r.cfg.Providers {
		if p.Name == name {
			return p, true
		}
	}
	return ProviderConfig{}, false
}

// Routes is the editable routing: a global default and per-task overrides.
type Routes struct {
	Default string            `json:"default"`
	Tasks   map[string]string `json:"tasks"`
}

// Routes returns the current routing.
func (r *Registry) Routes() Routes {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := map[string]string{}
	for k, v := range r.cfg.Tasks {
		t[k] = v
	}
	return Routes{Default: r.cfg.Default, Tasks: t}
}

// SetRoutes replaces the routing after validating names. An empty task
// entry means "use the default".
func (r *Registry) SetRoutes(rt Routes) error {
	if _, ok := r.impl[rt.Default]; !ok {
		return fmt.Errorf("default provider %q is not configured", rt.Default)
	}
	tasks := map[string]string{}
	for task, name := range rt.Tasks {
		if !slices.Contains(Tasks, task) {
			return fmt.Errorf("unknown task %q", task)
		}
		if name == "" {
			continue
		}
		if _, ok := r.impl[name]; !ok {
			return fmt.Errorf("task %q routes to unknown provider %q", task, name)
		}
		if cfg, _ := r.Config(name); slices.Contains(LocalTasks, task) && !cfg.Local() {
			return fmt.Errorf("task %q runs only on a local model; %q is not local", task, name)
		}
		tasks[task] = name
	}
	r.mu.Lock()
	r.cfg.Default, r.cfg.Tasks = rt.Default, tasks
	r.mu.Unlock()
	return nil
}

// ProviderFor returns the provider routed to task, or override when set. A
// local-only task never resolves to a cloud provider.
func (r *Registry) ProviderFor(task, override string) (string, error) {
	name := override
	explicit := name != ""
	r.mu.Lock()
	if name == "" {
		name = r.cfg.Tasks[task]
		explicit = name != ""
	}
	local := slices.Contains(LocalTasks, task)
	if name == "" && local {
		name = r.firstLocalLocked()
		if name == "" {
			r.mu.Unlock()
			return "", ErrNoLocalModel
		}
	}
	if name == "" {
		name = r.cfg.Default
	}
	r.mu.Unlock()
	if name == "" {
		return "", errors.New("no AI provider configured; add one to config/llm.yaml or set CODESYNAPSE_LLM_BASE_URL")
	}
	if _, ok := r.impl[name]; !ok {
		return "", fmt.Errorf("unknown AI provider %q", name)
	}
	if local && explicit {
		if cfg, ok := r.Config(name); !ok || !cfg.Local() {
			return "", fmt.Errorf("task %q runs only on a local model; %q is not local", task, name)
		}
	}
	return name, nil
}

// firstLocalLocked prefers the default provider when it is local, then the
// first local one in configuration order. The caller holds r.mu.
func (r *Registry) firstLocalLocked() string {
	for _, p := range r.cfg.Providers {
		if p.Name == r.cfg.Default && p.Local() {
			return p.Name
		}
	}
	for _, p := range r.cfg.Providers {
		if p.Local() {
			return p.Name
		}
	}
	return ""
}

// TaskRoutes returns the provider used for each task.
func (r *Registry) TaskRoutes() map[string]string {
	out := map[string]string{}
	for _, t := range Tasks {
		if name, err := r.ProviderFor(t, ""); err == nil {
			out[t] = name
		}
	}
	return out
}

// Usage returns a snapshot of usage for name.
func (r *Registry) Usage(name string) Usage {
	r.mu.Lock()
	defer r.mu.Unlock()
	if u, ok := r.usage[name]; ok {
		return *u
	}
	return Usage{}
}

// Complete sends req to provider name. Prompts are redacted again here
// (secrets and personal data) so no caller can forget it, and answers are
// capped by the provider limit.
func (r *Registry) Complete(ctx context.Context, name string, req Request) (Response, error) {
	if _, err := execution.Require(ctx); err != nil {
		return Response{}, err
	}
	p, ok := r.impl[name]
	if !ok {
		return Response{}, fmt.Errorf("unknown AI provider %q", name)
	}
	req, cfg, count, err := r.prepare(ctx, name, req)
	if err != nil {
		return Response{Preflight: count}, err
	}
	ctx = telemetry.WithModel(ctx, name, cfg.Model)
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	// Configured provider and model only; never the prompt or the answer.
	ctx, span := telemetry.Tracer().Start(ctx, "llm complete", trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("gen_ai.provider.name", name), attribute.String("gen_ai.request.model", cfg.Model)))
	defer span.End()
	start := time.Now()
	resp, err := p.Complete(ctx, req)
	if err != nil {
		span.SetStatus(codes.Error, "completion failed")
	}
	span.SetAttributes(attribute.Int("gen_ai.usage.input_tokens", resp.InputTokens), attribute.Int("gen_ai.usage.output_tokens", resp.OutputTokens))
	resp.Preflight = count
	resp.Duration = time.Since(start)
	record(ctx, name, cfg, resp, err)
	r.mu.Lock()
	u := r.usage[name]
	u.AvgSeconds = (u.AvgSeconds*float64(u.Requests) + resp.Duration.Seconds()) / float64(u.Requests+1)
	u.Requests++
	u.InputTokens += max(resp.InputTokens, 0)
	u.OutputTokens += max(resp.OutputTokens, 0)
	if err != nil {
		u.Failures++
		u.LastError = err.Error()
	} else {
		u.LastError = ""
	}
	r.mu.Unlock()
	return resp, err
}

// Cost estimates dollars using configured token rates. Callers must check
// price availability before publishing an estimate.
func Cost(cfg ProviderConfig, resp Response) float64 {
	cachedPrice, writePrice := cfg.PriceCachedInputPerMTok, cfg.PriceCacheWritePerMTok
	if !cfg.PricingConfigured {
		if cachedPrice == 0 {
			cachedPrice = cfg.PriceInputPerMTok
		}
		if writePrice == 0 {
			writePrice = cfg.PriceInputPerMTok
		}
	}
	input := max(resp.InputTokens, 0)
	cached := min(max(resp.CachedInputTokens, 0), input)
	written := min(max(resp.CacheWriteInputTokens, 0), input-cached)
	fresh := input - cached - written
	return (float64(fresh)*cfg.PriceInputPerMTok + float64(cached)*cachedPrice + float64(written)*writePrice + float64(max(resp.OutputTokens, 0))*cfg.PriceOutputPerMTok) / 1e6
}

func pricesKnown(cfg ProviderConfig, resp Response) bool {
	if cfg.PricingConfigured {
		return true
	}
	if cfg.Local() && cfg.PriceInputPerMTok == 0 && cfg.PriceOutputPerMTok == 0 && cfg.PriceCachedInputPerMTok == 0 && cfg.PriceCacheWritePerMTok == 0 {
		return true
	}
	return (resp.InputTokens == 0 || cfg.PriceInputPerMTok > 0) &&
		(resp.OutputTokens == 0 || cfg.PriceOutputPerMTok > 0) &&
		(resp.CachedInputTokens == 0 || cfg.PriceCachedInputPerMTok > 0) &&
		(resp.CacheWriteInputTokens == 0 || cfg.PriceCacheWritePerMTok > 0)
}

// record uses configured model identities, never provider response labels.
func record(ctx context.Context, name string, cfg ProviderConfig, resp Response, err error) {
	ctx = telemetry.WithModel(ctx, name, cfg.Model)
	tenant, task := telemetry.Tenant(ctx), taskOf(ctx)
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	telemetry.LLMRequests.IncContext(ctx, tenant, name, cfg.Model, task, outcome)
	telemetry.LLMDuration.ObserveContext(ctx, resp.Duration.Seconds(), tenant, name, cfg.Model)
	if !resp.UsageKnown {
		telemetry.LLMUsageUnknown.IncContext(ctx, tenant, name, cfg.Model)
	}
	if resp.UsageReported {
		telemetry.LLMInput.AddContext(ctx, float64(max(resp.InputTokens, 0)), tenant, name, cfg.Model, task)
		telemetry.LLMOutput.AddContext(ctx, float64(max(resp.OutputTokens, 0)), tenant, name, cfg.Model, task)
		telemetry.LLMSaved.AddContext(ctx, float64(min(max(resp.CachedInputTokens, 0), max(resp.InputTokens, 0))), tenant, name, cfg.Model, task)
	}
	if resp.UsageKnown {
		if pricesKnown(cfg, resp) {
			cost := Cost(cfg, resp)
			telemetry.LLMCost.AddContext(ctx, cost, tenant, name, cfg.Model)
			baseline := (float64(max(resp.InputTokens, 0))*cfg.PriceInputPerMTok + float64(max(resp.OutputTokens, 0))*cfg.PriceOutputPerMTok) / 1e6
			telemetry.LLMCostSaved.AddContext(ctx, max(baseline-cost, 0), tenant, name, cfg.Model)
		} else {
			telemetry.LLMPriceMissing.IncContext(ctx, tenant, name, cfg.Model)
		}
	}
	if resp.FirstToken > 0 {
		telemetry.LLMTTFT.ObserveContext(ctx, resp.FirstToken.Seconds(), tenant, name, cfg.Model)
	}
}

// Embedding limits per call: entity names and queries are short.
const (
	maxEmbedInputs = 64
	maxEmbedChars  = 512
)

// ErrNoEmbeddings means no local provider with an embedding model is routed
// to the "embeddings" task.
var ErrNoEmbeddings = errors.New("no local embedding model: set embedding_model on a local provider and route \"embeddings\" to it")

type embedder interface {
	Embed(ctx context.Context, model string, inputs []string) ([][]float32, error)
}

// EmbeddingProvider returns the provider routed to "embeddings" when it is
// local and has an embedding model. Memory names and queries never go to a
// cloud provider.
func (r *Registry) EmbeddingProvider() (string, ProviderConfig, error) {
	name, err := r.ProviderFor("embeddings", "")
	if err != nil {
		return "", ProviderConfig{}, ErrNoEmbeddings
	}
	cfg, ok := r.Config(name)
	if _, can := r.impl[name].(embedder); !ok || !can || !cfg.Local() || cfg.EmbeddingModel == "" {
		return "", ProviderConfig{}, ErrNoEmbeddings
	}
	return name, cfg, nil
}

// Embed returns one vector per input from provider name, inputs redacted
// and truncated.
func (r *Registry) Embed(ctx context.Context, name string, inputs []string) ([][]float32, error) {
	if _, err := execution.Require(ctx); err != nil {
		return nil, err
	}
	p, ok := r.impl[name].(embedder)
	cfg, _ := r.Config(name)
	if !ok || cfg.EmbeddingModel == "" {
		return nil, ErrNoEmbeddings
	}
	if len(inputs) == 0 || len(inputs) > maxEmbedInputs {
		return nil, fmt.Errorf("embed 1 to %d inputs", maxEmbedInputs)
	}
	clean := make([]string, len(inputs))
	for i, in := range inputs {
		in = redact.Free(in)
		if rs := []rune(in); len(rs) > maxEmbedChars {
			in = string(rs[:maxEmbedChars])
		}
		clean[i] = in
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(max(cfg.TimeoutSeconds, 10))*time.Second)
	defer cancel()
	ctx = telemetry.WithModel(ctx, name, cfg.EmbeddingModel)
	vecs, err := p.Embed(ctx, cfg.EmbeddingModel, clean)
	if err == nil && len(vecs) != len(inputs) {
		err = fmt.Errorf("%s: %d embeddings for %d inputs", name, len(vecs), len(inputs))
	}
	return vecs, err
}

// Models lists what provider name serves.
func (r *Registry) Models(ctx context.Context, name string) ([]string, error) {
	p, ok := r.impl[name]
	if !ok {
		return nil, fmt.Errorf("unknown AI provider %q", name)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return p.Models(ctx)
}

var thinkBlock = regexp.MustCompile(`(?s)<think>.*?</think>`)

// CleanText strips reasoning blocks some local models put in their answer.
func CleanText(s string) string {
	return strings.TrimSpace(thinkBlock.ReplaceAllString(s, ""))
}
