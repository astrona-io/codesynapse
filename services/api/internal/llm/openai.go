package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// openAI speaks the OpenAI-compatible chat completions API, which MLX
// (mlx_lm.server), Ollama (/v1), vLLM, LM Studio, llama.cpp server and most
// cloud providers implement.
type openAI struct {
	cfg  ProviderConfig
	http *http.Client
}

func newOpenAI(cfg ProviderConfig) *openAI {
	return &openAI{cfg: cfg, http: &http.Client{Transport: modelTransport(cfg, nil)}}
}

const maxResponseBytes = 8 << 20

func (o *openAI) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, o.cfg.BaseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.cfg.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.cfg.apiKey)
	}
	res, err := o.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", o.cfg.Name, err)
	}
	defer res.Body.Close() //nolint:errcheck // read-only
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	if err != nil {
		return err
	}
	if res.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return fmt.Errorf("%s: HTTP %d: %s", o.cfg.Name, res.StatusCode, msg)
	}
	return json.Unmarshal(data, out)
}

func (o *openAI) Complete(ctx context.Context, req Request) (Response, error) {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	body := map[string]any{
		"messages":    []msg{{"system", req.System}, {"user", req.User}},
		"max_tokens":  req.MaxTokens,
		"temperature": 0.2,
		"stream":      false,
	}
	if o.cfg.Model != "" {
		body["model"] = o.cfg.Model
	}
	if o.cfg.DisableThinking {
		body["chat_template_kwargs"] = map[string]any{"enable_thinking": false}
	}
	var res struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        *int `json:"prompt_tokens"`
			CompletionTokens    *int `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := o.do(ctx, http.MethodPost, "/chat/completions", body, &res); err != nil {
		return Response{}, err
	}
	resp := Response{Model: o.cfg.Model}
	if res.Model != "" {
		resp.Model = res.Model
	}
	resp.UsageReported = res.Usage.PromptTokens != nil || res.Usage.CompletionTokens != nil
	resp.UsageKnown = res.Usage.PromptTokens != nil && res.Usage.CompletionTokens != nil
	if res.Usage.PromptTokens != nil {
		resp.InputTokens = max(*res.Usage.PromptTokens, 0)
		resp.UsageKnown = resp.UsageKnown && *res.Usage.PromptTokens >= 0
	}
	if res.Usage.CompletionTokens != nil {
		resp.OutputTokens = max(*res.Usage.CompletionTokens, 0)
		resp.UsageKnown = resp.UsageKnown && *res.Usage.CompletionTokens >= 0
	}
	resp.UsageKnown = resp.UsageKnown && res.Usage.PromptTokensDetails.CachedTokens >= 0 && res.Usage.PromptTokensDetails.CachedTokens <= resp.InputTokens
	if res.Usage.PromptTokensDetails.CachedTokens >= 0 && res.Usage.PromptTokensDetails.CachedTokens <= resp.InputTokens {
		resp.CachedInputTokens = res.Usage.PromptTokensDetails.CachedTokens
	}
	if len(res.Choices) == 0 {
		return resp, fmt.Errorf("%s: empty response", o.cfg.Name)
	}
	resp.Text = CleanText(res.Choices[0].Message.Content)
	if resp.Text == "" && res.Choices[0].FinishReason == "length" {
		return resp, fmt.Errorf("%s: answer cut off at max_output_tokens=%d before any output; raise it for reasoning models", o.cfg.Name, req.MaxTokens)
	}
	return resp, nil
}

func (o *openAI) Models(ctx context.Context) ([]string, error) {
	var res struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := o.do(ctx, http.MethodGet, "/models", nil, &res); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(res.Data))
	for _, m := range res.Data {
		out = append(out, m.ID)
	}
	return out, nil
}

// Embed calls the OpenAI-compatible /embeddings endpoint (Ollama, LM Studio,
// vLLM, llama.cpp server).
func (o *openAI) Embed(ctx context.Context, model string, inputs []string) ([][]float32, error) {
	var res struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := o.do(ctx, http.MethodPost, "/embeddings", map[string]any{"model": model, "input": inputs}, &res); err != nil {
		return nil, err
	}
	out := make([][]float32, len(inputs))
	for _, d := range res.Data {
		if d.Index < 0 || d.Index >= len(out) || len(d.Embedding) == 0 || len(d.Embedding) > 8192 {
			return nil, fmt.Errorf("%s: unexpected embedding at index %d", o.cfg.Name, d.Index)
		}
		out[d.Index] = d.Embedding
	}
	for i, v := range out {
		if v == nil {
			return nil, fmt.Errorf("%s: no embedding for input %d", o.cfg.Name, i)
		}
	}
	return out, nil
}
