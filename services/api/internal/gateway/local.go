package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	// RouteHeader tells the agent where the answer came from.
	RouteHeader = "X-CodeSynapse-Route"
	// maxLocalBody: bigger requests are agent turns with long context, which
	// the main model should read.
	maxLocalBody = 256 << 10
	// defaultWait is how long a request queues for the local model before the
	// gateway sends it to the cloud instead.
	defaultWait = 20 * time.Second
)

// Usage is what the local model reported for one answer; zero when it
// reported nothing.
type Usage struct{ In, Out int }

// Local answers small, tool-free requests from a local OpenAI-compatible
// model. Anything else goes to the cloud.
type Local struct {
	BaseURL string
	Model   string
	APIKey  string
	Client  *http.Client
	// Eligible says whether a requested model name is a small background
	// model that the local one may replace; nil means SmallModel.
	Eligible func(model string) bool
	// Acquire takes the local model's queue slot and returns its release.
	Acquire func(ctx context.Context) (func(), error)
	// Wait bounds the time in the queue; 0 means defaultWait.
	Wait time.Duration
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

// translate builds the OpenAI-compatible request; ok is false when the
// request is not one the local model should take.
func (l *Local) translate(body []byte) (in anthropicIn, out []byte, ok bool) {
	if len(body) > maxLocalBody || json.Unmarshal(body, &in) != nil {
		return in, nil, false
	}
	elig := l.Eligible
	if elig == nil {
		elig = SmallModel
	}
	if !elig(in.Model) || len(in.Messages) == 0 || len(in.Tools) > 0 || present(in.ToolChoice) ||
		(in.Thinking.Type != "" && in.Thinking.Type != "disabled") {
		return in, nil, false
	}
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	var msgs []msg
	if present(in.System) {
		sys, ok := text(in.System)
		if !ok {
			return in, nil, false
		}
		msgs = append(msgs, msg{"system", sys})
	}
	for _, m := range in.Messages {
		t, ok := text(m.Content)
		if !ok || (m.Role != "user" && m.Role != "assistant") {
			return in, nil, false
		}
		msgs = append(msgs, msg{m.Role, t})
	}
	// include_usage makes the server report the tokens it used, so the
	// gateway can say what a local answer saved.
	req := map[string]any{"model": l.Model, "messages": msgs, "stream": true, "stream_options": map[string]bool{"include_usage": true}}
	if in.MaxTokens > 0 {
		req["max_tokens"] = in.MaxTokens
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
	return in, out, err == nil
}

// serve tries to answer from the local model. It returns false, with nothing
// written, when the cloud should take the request.
func (l *Local) serve(ctx context.Context, w http.ResponseWriter, body []byte) (Usage, bool) {
	in, req, ok := l.translate(body)
	if !ok {
		return Usage{}, false
	}
	release := func() {}
	if l.Acquire != nil {
		wait := l.Wait
		if wait <= 0 {
			wait = defaultWait
		}
		qctx, cancel := context.WithTimeout(ctx, wait)
		var err error
		release, err = l.Acquire(qctx)
		cancel()
		if err != nil {
			return Usage{}, false
		}
	}
	defer release()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(l.BaseURL, "/")+"/chat/completions", bytes.NewReader(req))
	if err != nil {
		return Usage{}, false
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if l.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+l.APIKey)
	}
	resp, err := l.client().Do(httpReq)
	if err != nil {
		return Usage{}, false
	}
	defer resp.Body.Close() //nolint:errcheck // read side
	if resp.StatusCode != http.StatusOK {
		return Usage{}, false
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	next := func() (chunk, bool) { return readChunk(sc) }
	// Wait for the first chunk before writing anything, so a model that
	// fails to start still falls through to the cloud.
	first, more := next()
	if !more && sc.Err() != nil {
		return Usage{}, false
	}
	w.Header().Set(RouteHeader, "local")
	var u Usage
	if in.Stream {
		u = streamAnswer(w, in.Model, first, more, next)
	} else {
		u = jsonAnswer(w, in.Model, first, more, next)
	}
	return u, true
}

type chunk struct {
	Text   string
	Finish string
	In     int
	Out    int
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
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				Prompt     int `json:"prompt_tokens"`
				Completion int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &v) != nil {
			continue
		}
		var c chunk
		if len(v.Choices) > 0 {
			c.Text, c.Finish = v.Choices[0].Delta.Content, v.Choices[0].FinishReason
		}
		if v.Usage != nil {
			c.In, c.Out = v.Usage.Prompt, v.Usage.Completion
		}
		return c, true
	}
	return chunk{}, false
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
	var inTok, outTok int
	for c, ok := first, more; ok; c, ok = next() {
		if c.Text != "" {
			send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": c.Text}})
		}
		if c.Finish != "" {
			finish = c.Finish
		}
		if c.In > 0 || c.Out > 0 {
			inTok, outTok = c.In, c.Out
		}
	}
	send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	send("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason(finish), "stop_sequence": nil}, "usage": map[string]int{"input_tokens": inTok, "output_tokens": outTok}})
	send("message_stop", map[string]string{"type": "message_stop"})
	return Usage{In: inTok, Out: outTok}
}

// jsonAnswer writes the single Anthropic message for the chunks.
func jsonAnswer(w http.ResponseWriter, model string, first chunk, more bool, next func() (chunk, bool)) Usage {
	var b strings.Builder
	var finish string
	var inTok, outTok int
	for c, ok := first, more; ok; c, ok = next() {
		b.WriteString(c.Text)
		if c.Finish != "" {
			finish = c.Finish
		}
		if c.In > 0 || c.Out > 0 {
			inTok, outTok = c.In, c.Out
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": messageID(), "type": "message", "role": "assistant", "model": model,
		"content":     []map[string]string{{"type": "text", "text": b.String()}},
		"stop_reason": stopReason(finish), "stop_sequence": nil,
		"usage": map[string]int{"input_tokens": inTok, "output_tokens": outTok},
	})
	return Usage{In: inTok, Out: outTok}
}
