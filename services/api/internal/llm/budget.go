package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/redact"
)

// MaxPromptBytes bounds combined system/user text before and after redaction.
const MaxPromptBytes = 1 << 20

// JSON escaping can expand a byte to six bytes; the envelope has its own cap.
const maxPromptEnvelopeBytes = 4 << 20

// Admission errors do not imply that a provider request or billable usage occurred.
var (
	ErrInputBudget           = errors.New("prompt exceeds input budget")
	ErrExactCountUnavailable = errors.New("exact prompt count is unavailable")
)

// InputCount describes admission accounting, never provider-reported billing.
// Bytes is the size of the canonical request JSON, not HTTP framing or traffic.
type InputCount struct {
	Tokens   int    `json:"tokens"`
	Bytes    int    `json:"bytes"`
	Model    string `json:"model"`
	Encoding string `json:"encoding"`
	Version  string `json:"version"`
	Exact    bool   `json:"exact"`
}

// BudgetError contains counts and limits only; rejected prompts stay private.
type BudgetError struct {
	Unit  string
	Count int
	Limit int
}

func (e *BudgetError) Error() string {
	return fmt.Sprintf("%s: %d %s exceeds %d", ErrInputBudget, e.Count, e.Unit, e.Limit)
}

func (e *BudgetError) Unwrap() error { return ErrInputBudget }

// promptCounter can be replaced only when a model's complete message template
// and tokenizer are verified together. A text tokenizer alone is insufficient.
type promptCounter interface {
	Count(context.Context, ProviderConfig, Request) (InputCount, error)
}

type requestByteCounter struct{}

func (requestByteCounter) Count(ctx context.Context, cfg ProviderConfig, req Request) (InputCount, error) {
	if err := ctx.Err(); err != nil {
		return InputCount{}, err
	}
	var body any
	switch cfg.Kind {
	case "", KindOpenAI:
		body = openAIRequest(cfg, req)
	case KindAnthropic:
		// The SDK adds stream=true after constructing the shared parameters.
		params := anthropicRequest(cfg, req)
		encoded, err := json.Marshal(params)
		if err != nil {
			return InputCount{}, errors.New("cannot encode provider prompt")
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil {
			return InputCount{}, errors.New("cannot encode provider prompt")
		}
		fields["stream"] = json.RawMessage("true")
		body = fields
	default:
		return InputCount{}, errors.New("no prompt counter for provider kind")
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return InputCount{}, errors.New("cannot encode provider prompt")
	}
	if err := ctx.Err(); err != nil {
		return InputCount{}, err
	}
	count := InputCount{Tokens: (len(encoded) + max(cfg.BytesPerToken, 1) - 1) / max(cfg.BytesPerToken, 1), Bytes: len(encoded), Model: cfg.Model, Encoding: "utf8-bytes", Version: "request-json-v1", Exact: false}
	if len(encoded) > maxPromptEnvelopeBytes {
		return count, &BudgetError{Unit: "request bytes", Count: len(encoded), Limit: maxPromptEnvelopeBytes}
	}
	return count, nil
}

func validateBudgetConfig(p ProviderConfig) error {
	if len(p.Model) > 4096 {
		return errors.New("model name exceeds 4096 bytes")
	}
	if len(p.Effort) > 64 {
		return errors.New("provider effort exceeds 64 bytes")
	}
	if p.MaxInputTokens < 0 || p.MaxOutputTokens < 0 || p.ContextWindow < 0 || p.TimeoutSeconds < 0 || p.TimeoutSeconds > 86400 {
		return errors.New("model budgets must be nonnegative and timeout_seconds must not exceed 86400")
	}
	if p.BytesPerToken < 0 || p.BytesPerToken > 8 {
		return errors.New("bytes_per_token must be between 1 and 8")
	}
	if p.InputBudgetPolicy != "" && p.InputBudgetPolicy != "estimate" && p.InputBudgetPolicy != "require_exact" {
		return errors.New("input_budget_policy must be estimate or require_exact")
	}
	return nil
}

func effectiveBudgets(p ProviderConfig) (ProviderConfig, error) {
	if err := validateBudgetConfig(p); err != nil {
		return p, err
	}
	budgets(&p)
	if p.ContextWindow > 0 && (p.MaxOutputTokens >= p.ContextWindow || p.ContextWindow-p.MaxOutputTokens <= contextMargin || p.MaxInputTokens > p.ContextWindow-p.MaxOutputTokens-contextMargin) {
		return p, errors.New("context_window cannot fit input, output and the template reserve")
	}
	return p, nil
}

// Preflight checks the same redacted request that Complete will send. It neither
// invokes a provider nor records usage. Callers may use it to pack whole items.
func (r *Registry) Preflight(ctx context.Context, name string, req Request) (InputCount, error) {
	_, _, count, err := r.prepare(ctx, name, req)
	return count, err
}

func (r *Registry) prepare(ctx context.Context, name string, req Request) (Request, ProviderConfig, InputCount, error) {
	var empty InputCount
	if _, err := execution.Require(ctx); err != nil {
		return req, ProviderConfig{}, empty, err
	}
	if err := ctx.Err(); err != nil {
		return req, ProviderConfig{}, empty, err
	}
	cfg, ok := r.Config(name)
	if !ok {
		return req, cfg, empty, fmt.Errorf("unknown AI provider %q", name)
	}
	cfg, err := effectiveBudgets(cfg)
	if err != nil {
		return req, cfg, empty, err
	}
	if len(req.System) > MaxPromptBytes || len(req.User) > MaxPromptBytes-len(req.System) {
		return req, cfg, empty, &BudgetError{Unit: "prompt bytes", Count: MaxPromptBytes + 1, Limit: MaxPromptBytes}
	}
	if !utf8.ValidString(req.System) || !utf8.ValidString(req.User) {
		return req, cfg, empty, errors.New("prompt must contain valid UTF-8")
	}
	req.System, req.User = redact.Free(req.System), redact.Free(req.User)
	if len(req.System) > MaxPromptBytes || len(req.User) > MaxPromptBytes-len(req.System) {
		return req, cfg, empty, &BudgetError{Unit: "redacted prompt bytes", Count: MaxPromptBytes + 1, Limit: MaxPromptBytes}
	}
	if req.MaxTokens <= 0 || req.MaxTokens > cfg.MaxOutputTokens {
		req.MaxTokens = cfg.MaxOutputTokens
	}
	counter := r.counter
	if counter == nil {
		counter = requestByteCounter{}
	}
	count, err := counter.Count(ctx, cfg, req)
	if err != nil {
		return req, cfg, count, err
	}
	if count.Tokens < 0 || count.Bytes < 0 || count.Model != cfg.Model || count.Encoding == "" || count.Version == "" {
		return req, cfg, InputCount{}, errors.New("invalid prompt counter result")
	}
	if count.Bytes > maxPromptEnvelopeBytes {
		return req, cfg, count, &BudgetError{Unit: "request bytes", Count: count.Bytes, Limit: maxPromptEnvelopeBytes}
	}
	if cfg.InputBudgetPolicy == "require_exact" && !count.Exact {
		return req, cfg, count, ErrExactCountUnavailable
	}
	limit := cfg.MaxInputTokens
	if cfg.ContextWindow > 0 {
		limit = min(limit, cfg.ContextWindow-req.MaxTokens-contextMargin)
	}
	if count.Tokens > limit {
		return req, cfg, count, &BudgetError{Unit: "estimated input tokens", Count: count.Tokens, Limit: limit}
	}
	return req, cfg, count, nil
}
