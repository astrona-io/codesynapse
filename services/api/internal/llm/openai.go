package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// openAI speaks the OpenAI-compatible chat completions API, which MLX
// (mlx_lm.server), Ollama (/v1), vLLM, LM Studio, llama.cpp server and most
// cloud providers implement.
type openAI struct {
	cfg  ProviderConfig
	http *http.Client
	// noStream is set once a local server refused a streamed request, so
	// it is asked without streaming from then on.
	noStream atomic.Bool
}

func newOpenAI(cfg ProviderConfig) *openAI {
	return &openAI{cfg: cfg, http: &http.Client{Transport: modelTransport(cfg, nil)}}
}

const maxResponseBytes = 8 << 20

func (o *openAI) do(ctx context.Context, method, path string, body any, out any) error {
	res, err := o.send(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer res.Body.Close() //nolint:errcheck // read-only
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// httpError is a non-2xx answer; a 4xx to a streamed request means the
// server does not take the streaming options.
type httpError struct {
	name   string
	status int
	msg    string
}

func (e *httpError) Error() string { return fmt.Sprintf("%s: HTTP %d: %s", e.name, e.status, e.msg) }

// send posts body and returns the open response when it is 2xx.
func (o *openAI) send(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, o.cfg.BaseURL+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.cfg.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.cfg.apiKey)
	}
	res, err := o.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", o.cfg.Name, err)
	}
	if res.StatusCode/100 != 2 {
		defer res.Body.Close()                                //nolint:errcheck // read-only
		data, _ := io.ReadAll(io.LimitReader(res.Body, 4096)) //nolint:errcheck // the status is the error
		msg := strings.TrimSpace(string(data))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, &httpError{name: o.cfg.Name, status: res.StatusCode, msg: msg}
	}
	return res, nil
}

func openAIRequest(cfg ProviderConfig, req Request) map[string]any {
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
	if cfg.Model != "" {
		body["model"] = cfg.Model
	}
	if cfg.Local() {
		// Streamed, so the time to the first token can be measured; Preflight
		// counts this shape, the larger of the two.
		body["stream"] = true
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if cfg.DisableThinking {
		body["chat_template_kwargs"] = map[string]any{"enable_thinking": false}
	}
	// Opt-in: vLLM refuses values it does not know, so disable_thinking
	// cannot send it for every server.
	if cfg.Effort != "" {
		body["reasoning_effort"] = cfg.Effort
	}
	return body
}

// openAIUsage is the usage block of an answer or of a stream's last chunk.
type openAIUsage struct {
	PromptTokens        *int `json:"prompt_tokens"`
	CompletionTokens    *int `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// Complete asks for one answer. Local servers are asked to stream it: the
// time to the first token separates reading the prompt from writing the
// answer, which Statistics shows per model. Cloud providers are asked
// without streaming, as before.
func (o *openAI) Complete(ctx context.Context, req Request) (Response, error) {
	if o.cfg.Local() && !o.noStream.Load() {
		resp, err := o.completeStream(ctx, req)
		var he *httpError
		if !errors.As(err, &he) || he.status/100 != 4 {
			return resp, err
		}
		o.noStream.Store(true)
	}
	var res openAIAnswer
	body := openAIRequest(o.cfg, req)
	body["stream"] = false
	delete(body, "stream_options")
	if err := o.do(ctx, http.MethodPost, "/chat/completions", body, &res); err != nil {
		return Response{}, err
	}
	return o.whole(res, req)
}

// openAIAnswer is a complete, unstreamed answer.
type openAIAnswer struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage openAIUsage `json:"usage"`
}

func (o *openAI) whole(res openAIAnswer, req Request) (Response, error) {
	text, finish := "", ""
	if len(res.Choices) > 0 {
		text, finish = res.Choices[0].Message.Content, res.Choices[0].FinishReason
	}
	return o.answer(cmpModel(res.Model, o.cfg.Model), res.Usage, len(res.Choices) > 0, text, finish, req)
}

func cmpModel(reported, configured string) string {
	if reported != "" {
		return reported
	}
	return configured
}

// completeStream reads a server-sent event stream with the usage chunk at
// the end (stream_options.include_usage).
func (o *openAI) completeStream(ctx context.Context, req Request) (Response, error) {
	body := openAIRequest(o.cfg, req)
	start := time.Now()
	res, err := o.send(ctx, http.MethodPost, "/chat/completions", body)
	if err != nil {
		return Response{}, err
	}
	defer res.Body.Close() //nolint:errcheck // read-only
	if !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		// The server ignored stream and answered whole.
		data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
		if err != nil {
			return Response{}, err
		}
		var whole openAIAnswer
		if err := json.Unmarshal(data, &whole); err != nil {
			return Response{}, fmt.Errorf("%s: %w", o.cfg.Name, err)
		}
		return o.whole(whole, req)
	}
	var (
		text   strings.Builder
		model  string
		finish string
		usage  openAIUsage
		first  time.Duration
		chosen bool
	)
	sc := bufio.NewScanner(io.LimitReader(res.Body, maxResponseBytes))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					Reasoning        string `json:"reasoning"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *openAIUsage `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return Response{}, fmt.Errorf("%s: unreadable stream chunk: %w", o.cfg.Name, err)
		}
		if chunk.Error != nil {
			return Response{}, fmt.Errorf("%s: %s", o.cfg.Name, chunk.Error.Message)
		}
		if chunk.Model != "" {
			model = chunk.Model
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		for _, c := range chunk.Choices {
			chosen = true
			d := c.Delta
			if first == 0 && d.Content+d.Reasoning+d.ReasoningContent != "" {
				first = time.Since(start)
			}
			text.WriteString(d.Content)
			if c.FinishReason != nil && *c.FinishReason != "" {
				finish = *c.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil {
		return Response{}, fmt.Errorf("%s: reading the stream: %w", o.cfg.Name, err)
	}
	resp, err := o.answer(cmpModel(model, o.cfg.Model), usage, chosen, text.String(), finish, req)
	resp.FirstToken = first
	return resp, err
}

// answer builds the Response from what either request returned.
func (o *openAI) answer(model string, u openAIUsage, chosen bool, text, finish string, req Request) (Response, error) {
	resp := Response{Model: model}
	res := struct{ Usage openAIUsage }{u}
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
	if !chosen {
		return resp, fmt.Errorf("%s: empty response", o.cfg.Name)
	}
	resp.Text = CleanText(text)
	if resp.Text == "" && finish == "length" {
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
