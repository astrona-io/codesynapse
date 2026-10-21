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
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/parisnakitakejser/codesynapse/internal/redact"
)

// Kinds of provider.
const (
	KindOpenAI    = "openai"    // any OpenAI-compatible /chat/completions server
	KindAnthropic = "anthropic" // Claude via the Anthropic SDK
)

// Tasks a provider can be routed to.
var Tasks = []string{"security-triage", "review", "briefing"}

// Request is one completion request.
type Request struct {
	System    string
	User      string
	MaxTokens int
}

// Response is a completion.
type Response struct {
	Text         string
	Model        string
	InputTokens  int
	OutputTokens int
	Duration     time.Duration
}

// Provider completes prompts.
type Provider interface {
	Complete(ctx context.Context, req Request) (Response, error)
	// Models lists model IDs the server offers (health check).
	Models(ctx context.Context) ([]string, error)
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
	MaxInputTokens int `yaml:"max_input_tokens" json:"max_input_tokens"`
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
	u := strings.ToLower(p.BaseURL)
	for _, h := range []string{"://localhost", "://127.", "://host.docker.internal", "://host.containers.internal", "://10.", "://192.168.", "://172.", ".local:", ".local/", ".lan"} {
		if strings.Contains(u, h) {
			return true
		}
	}
	return false
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
			if p.APIKeyEnv == "" {
				p.APIKeyEnv = "ANTHROPIC_API_KEY"
			}
		default:
			return fmt.Errorf("provider %q: kind must be %s or %s", p.Name, KindOpenAI, KindAnthropic)
		}
		if p.BaseURL != "" && !strings.HasPrefix(p.BaseURL, "http://") && !strings.HasPrefix(p.BaseURL, "https://") {
			return fmt.Errorf("provider %q: base_url must be http(s)", p.Name)
		}
		p.BaseURL = strings.TrimRight(p.BaseURL, "/")
		if p.MaxOutputTokens <= 0 {
			p.MaxOutputTokens = 4096
		}
		if p.MaxInputTokens <= 0 {
			p.MaxInputTokens = 32000
		}
		if p.TimeoutSeconds <= 0 {
			p.TimeoutSeconds = 300
		}
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
	cfg   Config
	impl  map[string]Provider
	mu    sync.Mutex
	usage map[string]*Usage
}

// NewRegistry builds providers from cfg.
func NewRegistry(cfg Config) *Registry {
	r := &Registry{cfg: cfg, impl: map[string]Provider{}, usage: map[string]*Usage{}}
	for _, p := range cfg.Providers {
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
		tasks[task] = name
	}
	r.mu.Lock()
	r.cfg.Default, r.cfg.Tasks = rt.Default, tasks
	r.mu.Unlock()
	return nil
}

// ProviderFor returns the provider routed to task, or override when set.
func (r *Registry) ProviderFor(task, override string) (string, error) {
	name := override
	r.mu.Lock()
	if name == "" {
		name = r.cfg.Tasks[task]
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
	return name, nil
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

// Complete sends req to provider name. Prompts are redacted again here so
// no caller can forget it, and answers are capped by the provider limit.
func (r *Registry) Complete(ctx context.Context, name string, req Request) (Response, error) {
	p, ok := r.impl[name]
	if !ok {
		return Response{}, fmt.Errorf("unknown AI provider %q", name)
	}
	cfg, _ := r.Config(name)
	req.System, req.User = redact.Text(req.System), redact.Text(req.User)
	if req.MaxTokens <= 0 || req.MaxTokens > cfg.MaxOutputTokens {
		req.MaxTokens = cfg.MaxOutputTokens
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	start := time.Now()
	resp, err := p.Complete(ctx, req)
	resp.Duration = time.Since(start)
	r.mu.Lock()
	u := r.usage[name]
	u.AvgSeconds = (u.AvgSeconds*float64(u.Requests) + resp.Duration.Seconds()) / float64(u.Requests+1)
	u.Requests++
	if err != nil {
		u.Failures++
		u.LastError = err.Error()
	} else {
		u.InputTokens += resp.InputTokens
		u.OutputTokens += resp.OutputTokens
		u.LastError = ""
	}
	r.mu.Unlock()
	return resp, err
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
