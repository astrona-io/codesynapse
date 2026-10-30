package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/modelpattern"
)

const (
	// RouteHeader tells the agent where the answer came from.
	RouteHeader = "X-CodeSynapse-Route"
	// maxLocalBody bounds what the local model is offered whatever its
	// context window, so a request held for it stays small in memory.
	maxLocalBody = 4 << 20
	// unknownWindowBody is the cap when the operator did not say how large
	// the local model's context is.
	unknownWindowBody = 256 << 10
	// defaultWait is how long a request queues for a free local slot before
	// the gateway sends it to the cloud instead (gateway_wait_seconds). The
	// answer itself is bounded by the provider's timeout_seconds.
	defaultWait = 60 * time.Second
)

// Usage is what the local model reported for one answer; zero when it
// reported nothing. Cached is the part of In its prompt cache served
// (prompt_tokens_details.cached_tokens, or llama.cpp's timings.cache_n).
// Wait is the time spent in the local model's queue and Answer the time from
// the slot to the answer, or to the point it fell through; Asked says the
// request got as far as the queue.
type Usage struct {
	In, Out, Cached int
	Wait, Answer    time.Duration
	Asked           bool
}

// Local answers requests that fit it, with or without tools, from a local
// OpenAI-compatible model. Anything else goes to the cloud.
type Local struct {
	BaseURL string
	Model   string
	APIKey  string
	Client  *http.Client
	// Eligible says whether a requested model name is one the local model
	// may replace; nil means every model unless the request's organisation
	// switched local first off (WithLocalFirst), SmallModel then.
	Eligible func(model string) bool
	// Exclude is the installation's list of model patterns that never go to
	// the local model (gateway_exclude in config/llm.yaml), normalized by
	// modelpattern. The organisation's own list (WithExclude) adds to it.
	Exclude []string
	// ContextWindow is the local model's context in tokens; a request whose
	// estimate does not fit goes to the cloud. 0 means unknown.
	ContextWindow int
	// Acquire takes the local model's queue slot and returns its release.
	Acquire func(ctx context.Context) (func(), error)
	// Wait bounds the time in the queue; 0 means defaultWait.
	Wait time.Duration
	// Effort is sent as reasoning_effort ("none" turns thinking off on
	// Ollama) and DisableThinking as chat_template_kwargs (vLLM). Without
	// them a reasoning model spends max_tokens on thinking and answers
	// nothing, which the gateway then sends to the cloud as truncated.
	Effort          string
	DisableThinking bool
}

// noRedirects is the client of a Local without one: the local model's address
// is fixed by the operator, so an answer that points elsewhere is not followed
// (its bearer key would go along).
var noRedirects = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (l *Local) client() *http.Client {
	if l.Client != nil {
		return l.Client
	}
	return noRedirects
}

// SmallModel is the default Eligible: Claude's small model class.
func SmallModel(model string) bool { return strings.Contains(strings.ToLower(model), "haiku") }

type localFirstKey struct{}

// WithLocalFirst records whether the request's organisation offers every
// model's requests to the local model first, not only the small model's.
// A request that carries no choice is local first: that is the default.
func WithLocalFirst(ctx context.Context, on bool) context.Context {
	return context.WithValue(ctx, localFirstKey{}, on)
}

func localFirst(ctx context.Context) bool {
	on, chose := ctx.Value(localFirstKey{}).(bool)
	return on || !chose
}

type localThinkingKey struct{}

// WithLocalThinking records whether the request's organisation offers
// requests that ask for thinking to the local model too. A request that
// carries no choice is offered: that is the default.
func WithLocalThinking(ctx context.Context, on bool) context.Context {
	return context.WithValue(ctx, localThinkingKey{}, on)
}

func localThinking(ctx context.Context) bool {
	on, chose := ctx.Value(localThinkingKey{}).(bool)
	return on || !chose
}

type excludeKey struct{}

// WithExclude records the organisation's model patterns (normalized by
// modelpattern) that must never be answered locally.
func WithExclude(ctx context.Context, patterns []string) context.Context {
	return context.WithValue(ctx, excludeKey{}, patterns)
}

// excluded says whether the installation or the request's organisation keeps
// model away from the local model. It wins over every other choice.
func (l *Local) excluded(ctx context.Context, model string) bool {
	org, _ := ctx.Value(excludeKey{}).([]string)
	return modelpattern.Match(l.Exclude, model) || modelpattern.Match(org, model)
}

// fits estimates the request at four bytes per token (JSON overcounts, which
// errs towards the cloud) plus the answer it asks room for.
func (l *Local) fits(size, maxTokens int) bool {
	if l.ContextWindow <= 0 {
		return size <= unknownWindowBody
	}
	return size <= maxLocalBody && size/4+max(maxTokens, 0) <= l.ContextWindow
}

type anthropicIn struct {
	Model         string            `json:"model"`
	MaxTokens     int               `json:"max_tokens"`
	System        json.RawMessage   `json:"system"`
	Messages      []anthropicMsg    `json:"messages"`
	Temperature   *float64          `json:"temperature"`
	TopP          *float64          `json:"top_p"`
	StopSequences []string          `json:"stop_sequences"`
	Stream        bool              `json:"stream"`
	Tools         []json.RawMessage `json:"tools"`
	ToolChoice    json.RawMessage   `json:"tool_choice"`
	Thinking      struct {
		Type string `json:"type"`
	} `json:"thinking"`
	// offered are the tools the local model is given: Tools without the
	// deferred ones no tool_reference has loaded yet. Set by translate.
	offered []json.RawMessage
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// text flattens content that is a string or text blocks. Any other block
// (image, tool use, tool result) makes it ineligible.
func text(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return "", false
	}
	var b strings.Builder
	for _, bl := range blocks {
		if bl.Type != "text" {
			return "", false
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(bl.Text)
	}
	return b.String(), true
}

func present(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

// Reasons a request offered to the local model went to the cloud instead:
// the fixed values of Meta.Fallthrough, mirrored in the telemetry allowlist
// (internal/telemetry/outcomes.go). Each says one thing, so the statistics
// can tell a slow model (queue_timeout, answer_timeout) from a broken one.
const (
	FallExcluded         = "excluded"          // the model is on the installation's or organisation's exclude list
	FallNotEligible      = "not_eligible"      // the model is not one the organisation answers locally
	FallBadRequest       = "bad_request"       // the body is not a request the gateway can read (bad JSON, no messages, unknown role)
	FallSystemNotText    = "system_not_text"   // the system prompt is not a string or text blocks
	FallUnsupportedBlock = "unsupported_block" // a message holds a block the local model cannot take (server tool results, unknown types)
	FallUnsupportedTool  = "unsupported_tool"  // a tool or tool_choice the local model cannot run, such as a server tool
	FallTooLarge         = "too_large"         // the request does not fit the local model's context
	FallHasThinking      = "has_thinking"      // extended thinking was asked for or sent back
	FallHasImage         = "has_image"         // the request carries an image or a document
	FallQueueTimeout     = "queue_timeout"     // no queue slot within the wait
	FallCancelled        = "cancelled"         // the agent went away while the request waited
	FallTimeout          = "answer_timeout"    // the local model took too long to answer
	FallModelError       = "model_error"       // the local server could not be reached or broke off
	FallBadStatus        = "bad_status"        // the local server answered with a status other than 200
	FallEmptyAnswer      = "empty_answer"      // a held answer had no text and no tool call
	FallTruncated        = "truncated"         // a held answer stopped at max tokens or ended without a finish
	FallBadAnswer        = "bad_answer"        // a held answer had a bad tool call or was too large to hold
	FallNoLocalModel     = "no_local_model"    // the gateway has no local model to offer the request to
	FallCountTokens      = "count_tokens"      // a token count: only the cloud counts with the model's own tokenizer
)

// failure names why a call to the local model failed: a deadline is a
// timeout, the agent leaving is a cancel, anything else a model error.
func failure(ctx context.Context, err error) string {
	var ne net.Error
	switch {
	case ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded):
		return FallCancelled
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return FallTimeout
	}
	return FallModelError
}

// heldFailure names why a held answer that is not complete was not kept: it
// stopped at max tokens or without a finish (truncated), or said nothing.
func heldFailure(a answer) string {
	if a.Finish != "length" && strings.TrimSpace(a.Text) == "" && len(a.Calls) == 0 {
		return FallEmptyAnswer
	}
	return FallTruncated
}

// contentKinds reports whether content holds an image or document block, or a
// thinking block, looking inside tool results too.
func contentKinds(raw json.RawMessage) (image, thinking bool) {
	var blocks []struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return false, false
	}
	for _, b := range blocks {
		switch b.Type {
		case "image", "document":
			image = true
		case "thinking", "redacted_thinking":
			thinking = true
		case "tool_result":
			i, t := contentKinds(b.Content)
			image, thinking = image || i, thinking || t
		}
	}
	return image, thinking
}

// translate builds the OpenAI-compatible request; when the request is not
// one the local model should take, out is nil and reason says why.
func (l *Local) translate(ctx context.Context, body []byte) (in anthropicIn, out []byte, reason string) {
	if len(body) > maxLocalBody {
		return in, nil, FallTooLarge
	}
	if json.Unmarshal(body, &in) != nil {
		return in, nil, FallBadRequest
	}
	if l.excluded(ctx, in.Model) {
		return in, nil, FallExcluded
	}
	if !l.fits(len(body), in.MaxTokens) {
		return in, nil, FallTooLarge
	}
	elig := l.Eligible
	if elig == nil {
		elig = SmallModel
		if localFirst(ctx) {
			elig = func(string) bool { return true }
		}
	}
	if !elig(in.Model) {
		return in, nil, FallNotEligible
	}
	if len(in.Messages) == 0 {
		return in, nil, FallBadRequest
	}
	// With local thinking on, the local model answers without thinking and
	// leaves Claude's earlier thinking blocks out: they are signed reasoning
	// it cannot read. The cloud still gets them unchanged when it answers,
	// and in adaptive mode a turn without thinking is not an error there.
	think := localThinking(ctx)
	if !think && in.Thinking.Type != "" && in.Thinking.Type != "disabled" {
		return in, nil, FallHasThinking
	}
	for _, m := range in.Messages {
		switch image, thinking := contentKinds(m.Content); {
		case image:
			return in, nil, FallHasImage
		case thinking && !think:
			return in, nil, FallHasThinking
		}
	}
	var msgs []oaMsg
	if present(in.System) {
		sys, ok := text(in.System)
		if !ok {
			return in, nil, FallSystemNotText
		}
		msgs = append(msgs, oaMsg{Role: "system", Content: sys})
	}
	for _, m := range in.Messages {
		conv, why := convertMessage(m, think)
		if why != "" {
			return in, nil, why
		}
		msgs = append(msgs, conv...)
	}
	in.offered = offeredTools(in)
	tools, choice, parallel, ok := convertTools(in.offered, in.ToolChoice)
	if !ok {
		return in, nil, FallUnsupportedTool
	}
	// include_usage makes the server report the tokens it used, so the
	// gateway can say what a local answer saved.
	req := map[string]any{"model": l.Model, "messages": msgs, "stream": true, "stream_options": map[string]bool{"include_usage": true}}
	if len(tools) > 0 {
		req["tools"] = tools
		if choice != nil {
			req["tool_choice"] = choice
		}
		if !parallel {
			req["parallel_tool_calls"] = false
		}
	}
	if in.MaxTokens > 0 {
		req["max_tokens"] = in.MaxTokens
	}
	if l.DisableThinking {
		req["chat_template_kwargs"] = map[string]any{"enable_thinking": false}
	}
	// Only when set: vLLM refuses values it does not know.
	if l.Effort != "" {
		req["reasoning_effort"] = l.Effort
	}
	if in.Temperature != nil {
		req["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		req["top_p"] = *in.TopP
	}
	if len(in.StopSequences) > 0 {
		req["stop"] = in.StopSequences
	}
	out, err := json.Marshal(req)
	if err != nil {
		return in, nil, FallBadRequest
	}
	return in, out, ""
}

// serve tries to answer from the local model. It returns false, with nothing
// written and one of the Fall* reasons, when the cloud should take the request.
func (l *Local) serve(ctx context.Context, w http.ResponseWriter, body []byte) (Usage, string, bool) {
	in, req, reason := l.translate(ctx, body)
	if req == nil {
		return Usage{}, reason, false
	}
	u := Usage{Asked: true}
	release := func() {}
	if l.Acquire != nil {
		wait := l.Wait
		if wait <= 0 {
			wait = defaultWait
		}
		queued := time.Now()
		qctx, cancel := context.WithTimeout(ctx, wait)
		var err error
		release, err = l.Acquire(qctx)
		cancel()
		u.Wait = time.Since(queued)
		if err != nil {
			if ctx.Err() != nil {
				return u, FallCancelled, false
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return u, FallQueueTimeout, false
			}
			return u, FallModelError, false
		}
	}
	defer release()
	started := time.Now()
	fail := func(reason string) (Usage, string, bool) {
		u.Answer = time.Since(started)
		return u, reason, false
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(l.BaseURL, "/")+"/chat/completions", bytes.NewReader(req))
	if err != nil {
		return fail(FallModelError)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if l.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+l.APIKey)
	}
	resp, err := l.client().Do(httpReq)
	if err != nil {
		return fail(failure(ctx, err))
	}
	defer resp.Body.Close() //nolint:errcheck // read side
	if resp.StatusCode != http.StatusOK {
		return fail(FallBadStatus)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	next := func() (chunk, bool) { return readChunk(sc) }
	answered := func(got Usage) (Usage, string, bool) {
		got.Wait, got.Answer, got.Asked = u.Wait, time.Since(started), true
		return got, "", true
	}
	// A request for a bigger model than the small one may ask more than the
	// local model can do: hold its answer and keep it only when it is whole.
	strict := !SmallModel(in.Model)
	if len(in.Tools) > 0 || strict {
		// A tool call is only usable whole: collect the answer, check it, and
		// let the cloud take the request if the local model got it wrong.
		first, more := next()
		ans, ok := collect(first, more, next, declaredTools(in.offered))
		if err := sc.Err(); err != nil {
			return fail(failure(ctx, err))
		}
		if !ok {
			return fail(FallBadAnswer)
		}
		if strict && !ans.keep(in.MaxTokens) {
			return fail(heldFailure(ans))
		}
		w.Header().Set(RouteHeader, "local")
		if in.Stream {
			return answered(streamCollected(w, in.Model, ans))
		}
		return answered(jsonCollected(w, in.Model, ans))
	}
	// Wait for the first chunk before writing anything, so a model that
	// fails to start still falls through to the cloud.
	first, more := next()
	if err := sc.Err(); !more && err != nil {
		return fail(failure(ctx, err))
	}
	w.Header().Set(RouteHeader, "local")
	if in.Stream {
		return answered(streamAnswer(w, in.Model, first, more, next))
	}
	return answered(jsonAnswer(w, in.Model, first, more, next))
}

type toolDelta struct {
	Index int
	ID    string
	Name  string
	Args  string
}

type chunk struct {
	Text   string
	Calls  []toolDelta
	Finish string
	In     int
	Out    int
	Cached int
}

// readChunk returns the next OpenAI stream chunk; false at the end.
func readChunk(sc *bufio.Scanner) (chunk, bool) {
	for sc.Scan() {
		data, found := strings.CutPrefix(strings.TrimSpace(sc.Text()), "data:")
		if !found {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			return chunk{}, false
		}
		var v struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				Prompt     int `json:"prompt_tokens"`
				Completion int `json:"completion_tokens"`
				Details    *struct {
					Cached int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
			Timings *struct {
				CacheN int `json:"cache_n"`
			} `json:"timings"`
		}
		if json.Unmarshal([]byte(data), &v) != nil {
			continue
		}
		var c chunk
		if len(v.Choices) > 0 {
			c.Text, c.Finish = v.Choices[0].Delta.Content, v.Choices[0].FinishReason
			for _, tc := range v.Choices[0].Delta.ToolCalls {
				c.Calls = append(c.Calls, toolDelta{tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments})
			}
		}
		if v.Usage != nil {
			c.In, c.Out = v.Usage.Prompt, v.Usage.Completion
			if v.Usage.Details != nil {
				c.Cached = v.Usage.Details.Cached
			}
		}
		if c.Cached == 0 && v.Timings != nil {
			c.Cached = v.Timings.CacheN
		}
		c.Cached = clampCached(c.Cached, c.In)
		return c, true
	}
	return chunk{}, false
}

// clampCached keeps a reported cache count within the prompt it is part of.
func clampCached(cached, in int) int {
	return max(0, min(cached, in))
}

func stopReason(finish string) string {
	if finish == "length" {
		return "max_tokens"
	}
	return "end_turn"
}

func messageID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "msg_cs_" + hex.EncodeToString(b)
}

// streamAnswer writes the Anthropic event stream for the chunks.
func streamAnswer(w http.ResponseWriter, model string, first chunk, more bool, next func() (chunk, bool)) Usage {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	send := func(event string, v any) {
		b, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": messageID(), "type": "message", "role": "assistant", "model": model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
	}})
	send("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}})
	var finish string
	var inTok, outTok, cachedTok int
	for c, ok := first, more; ok; c, ok = next() {
		if c.Text != "" {
			send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": c.Text}})
		}
		if c.Finish != "" {
			finish = c.Finish
		}
		if c.In > 0 || c.Out > 0 {
			inTok, outTok, cachedTok = c.In, c.Out, c.Cached
		}
	}
	send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason(finish), "stop_sequence": nil}, "usage": map[string]int{"input_tokens": inTok, "output_tokens": outTok}})
	send("message_stop", map[string]string{"type": "message_stop"})
	return Usage{In: inTok, Out: outTok, Cached: cachedTok}
}

// jsonAnswer writes the single Anthropic message for the chunks.
func jsonAnswer(w http.ResponseWriter, model string, first chunk, more bool, next func() (chunk, bool)) Usage {
	var b strings.Builder
	var finish string
	var inTok, outTok, cachedTok int
	for c, ok := first, more; ok; c, ok = next() {
		b.WriteString(c.Text)
		if c.Finish != "" {
			finish = c.Finish
		}
		if c.In > 0 || c.Out > 0 {
			inTok, outTok, cachedTok = c.In, c.Out, c.Cached
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": messageID(), "type": "message", "role": "assistant", "model": model,
		"content":     []map[string]string{{"type": "text", "text": b.String()}},
		"stop_reason": stopReason(finish), "stop_sequence": nil,
		"usage": map[string]int{"input_tokens": inTok, "output_tokens": outTok},
	})
	return Usage{In: inTok, Out: outTok, Cached: cachedTok}
}
