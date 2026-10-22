package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

// anthropicProvider calls Claude through the official SDK. Server-side
// refusal fallbacks are enabled so a declined request is re-served by a
// fallback model inside the same call.
type anthropicProvider struct {
	cfg    ProviderConfig
	client anthropic.Client
}

func newAnthropic(cfg ProviderConfig) *anthropicProvider {
	opts := []option.RequestOption{option.WithAPIKey(cfg.apiKey)}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	return &anthropicProvider{cfg: cfg, client: anthropic.NewClient(opts...)}
}

func (a *anthropicProvider) Complete(ctx context.Context, req Request) (Response, error) {
	if a.cfg.apiKey == "" {
		return Response{}, fmt.Errorf("%s: %s is not set", a.cfg.Name, a.cfg.APIKeyEnv)
	}
	params := anthropic.BetaMessageNewParams{
		Model:     a.cfg.Model,
		MaxTokens: int64(req.MaxTokens),
		System:    []anthropic.BetaTextBlockParam{{Text: req.System}},
		Messages: []anthropic.BetaMessageParam{
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(req.User)),
		},
		Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
		Fallbacks: anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()},
	}
	if a.cfg.Effort != "" {
		params.OutputConfig = anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(a.cfg.Effort)}
	}
	// Streamed so long answers are not cut by request timeouts.
	start := time.Now()
	stream := a.client.Beta.Messages.NewStreaming(ctx, params)
	var msg anthropic.BetaMessage
	var first time.Duration
	for stream.Next() {
		if first == 0 && stream.Current().Type == "content_block_delta" {
			first = time.Since(start)
		}
		if err := msg.Accumulate(stream.Current()); err != nil {
			return Response{}, fmt.Errorf("%s: %w", a.cfg.Name, err)
		}
	}
	if err := stream.Err(); err != nil {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			return Response{}, fmt.Errorf("%s: HTTP %d: %s", a.cfg.Name, apiErr.StatusCode, strings.TrimSpace(apiErr.Error()))
		}
		return Response{}, fmt.Errorf("%s: %w", a.cfg.Name, err)
	}
	switch msg.StopReason {
	case anthropic.BetaStopReasonRefusal:
		return Response{}, fmt.Errorf("%s: the model declined the request", a.cfg.Name)
	case anthropic.BetaStopReasonMaxTokens:
		// Keep what was produced; callers validate structure.
	}
	var b strings.Builder
	for _, block := range msg.Content {
		if t, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	return Response{
		Text: strings.TrimSpace(b.String()), Model: msg.Model,
		// Anthropic reports cache reads and writes apart from input_tokens.
		InputTokens:       int(msg.Usage.InputTokens + msg.Usage.CacheReadInputTokens + msg.Usage.CacheCreationInputTokens),
		CachedInputTokens: int(msg.Usage.CacheReadInputTokens), OutputTokens: int(msg.Usage.OutputTokens), FirstToken: first,
	}, nil
}

func (a *anthropicProvider) Models(ctx context.Context) ([]string, error) {
	if a.cfg.apiKey == "" {
		return nil, fmt.Errorf("%s: %s is not set", a.cfg.Name, a.cfg.APIKeyEnv)
	}
	page, err := a.client.Models.List(ctx, anthropic.ModelListParams{Limit: param.NewOpt[int64](20)})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", a.cfg.Name, err)
	}
	var out []string
	for _, m := range page.Data {
		out = append(out, m.ID)
	}
	return out, nil
}
