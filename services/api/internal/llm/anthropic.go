package llm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
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
	opts := []option.RequestOption{option.WithAPIKey(cfg.apiKey), option.WithHTTPClient(&http.Client{Transport: modelTransport(cfg, nil)})}
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
		// The system prompts are constants and come first, so they are the
		// cache prefix: batches of one task (triage runs several) read it
		// from the cache. The user part changes every call and stays after
		// the breakpoint. Prompts below the model's minimum are not cached.
		System: []anthropic.BetaTextBlockParam{{Text: req.System, CacheControl: anthropic.NewBetaCacheControlEphemeralParam()}},
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
	var inputKnown, outputKnown, stopped bool
	response := func() Response {
		usage := msg.Usage
		if usage.InputTokens < 0 || usage.CacheReadInputTokens < 0 || usage.CacheCreationInputTokens < 0 || usage.OutputTokens < 0 ||
			usage.InputTokens > math.MaxInt64-usage.CacheReadInputTokens || usage.InputTokens+usage.CacheReadInputTokens > math.MaxInt64-usage.CacheCreationInputTokens {
			return Response{Model: msg.Model, FirstToken: first}
		}
		return Response{Model: msg.Model, InputTokens: int(usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens), CachedInputTokens: int(usage.CacheReadInputTokens), CacheWriteInputTokens: int(usage.CacheCreationInputTokens), OutputTokens: int(usage.OutputTokens), FirstToken: first, UsageReported: inputKnown || outputKnown, UsageKnown: inputKnown && outputKnown && stopped}
	}
	defer stream.Close() //nolint:errcheck // read-only stream
	for stream.Next() {
		event := stream.Current()
		switch event.Type {
		case "message_start":
			inputKnown = event.Message.Usage.JSON.InputTokens.Valid()
			outputKnown = event.Message.Usage.JSON.OutputTokens.Valid()
		case "message_delta":
			inputKnown = inputKnown || event.Usage.JSON.InputTokens.Valid()
			outputKnown = outputKnown || event.Usage.JSON.OutputTokens.Valid()
		case "message_stop":
			stopped = true
		}
		if first == 0 && stream.Current().Type == "content_block_delta" {
			first = time.Since(start)
		}
		if err := msg.Accumulate(stream.Current()); err != nil {
			return response(), fmt.Errorf("%s: %w", a.cfg.Name, err)
		}
	}
	if err := stream.Err(); err != nil {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			return response(), fmt.Errorf("%s: HTTP %d: %s", a.cfg.Name, apiErr.StatusCode, strings.TrimSpace(apiErr.Error()))
		}
		return response(), fmt.Errorf("%s: %w", a.cfg.Name, err)
	}
	switch msg.StopReason {
	case anthropic.BetaStopReasonRefusal:
		return response(), fmt.Errorf("%s: the model declined the request", a.cfg.Name)
	case anthropic.BetaStopReasonMaxTokens:
		// Keep what was produced; callers validate structure.
	}
	var b strings.Builder
	for _, block := range msg.Content {
		if t, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	resp := response()
	resp.Text = strings.TrimSpace(b.String())
	return resp, nil
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
