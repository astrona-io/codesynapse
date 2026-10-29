package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// What the audit entry and, when an organisation turned it on, the prompt
// log learn about a request's text. The digest and size of the newest user
// text are always taken (the audit chain keeps them, never the text); the
// text itself and the local model's answer only when WithPromptCapture says
// the organisation keeps its prompt log. Tool results and system prompts are
// never part of it.

// maxAnswerCapture bounds the raw answer bytes held to read the local
// model's text from: event streams carry far more bytes than text.
const maxAnswerCapture = 1 << 20

type promptCaptureKey struct{}

// WithPromptCapture records whether the request's organisation keeps a
// prompt log, so the gateway holds on to the texts it needs for it.
func WithPromptCapture(ctx context.Context, on bool) context.Context {
	return context.WithValue(ctx, promptCaptureKey{}, on)
}

// PromptCapture reports what WithPromptCapture recorded.
func PromptCapture(ctx context.Context) bool { return promptCapture(ctx) }

func promptCapture(ctx context.Context) bool {
	on, _ := ctx.Value(promptCaptureKey{}).(bool)
	return on
}

func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// notePrompt records the digest and size of the newest user text, and the
// text itself (cut to accounts.PromptLogCapture) when capturing.
func (m *Meta) notePrompt(ctx context.Context, text string) {
	if text == "" {
		return
	}
	m.PromptSHA256, m.PromptBytes = digestOf(text), len(text)
	if promptCapture(ctx) {
		m.Prompt = clip(text, accounts.PromptLogCapture)
	}
}

// noteForwarded records what went to the cloud when the gateway changed the
// newest user text on the way (prompt shortening).
func (m *Meta) noteForwarded(ctx context.Context, sent, forwarded []byte) {
	if bytes.Equal(sent, forwarded) {
		return
	}
	text := anthropicUserText(forwarded)
	if text == "" || digestOf(text) == m.PromptSHA256 {
		return
	}
	m.TextChanged = true
	m.ForwardedSHA256, m.ForwardedBytes = digestOf(text), len(text)
	if promptCapture(ctx) {
		m.Forwarded = clip(text, accounts.PromptLogCapture)
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] // accounts.LogText cuts again on a character boundary
}

// userText joins the text blocks of one user message's content; tool
// results, images and any other block are left out.
func userText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// newestUserText is the text of the last user message that has any, so a
// turn that only returns tool results still names the prompt it serves.
func newestUserText(msgs []anthropicMsg) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "user" {
			continue
		}
		if t := userText(msgs[i].Content); t != "" {
			return t
		}
	}
	return ""
}

// anthropicUserText reads the newest user text of a Messages API body.
func anthropicUserText(body []byte) string {
	var v struct {
		Messages []anthropicMsg `json:"messages"`
	}
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	return newestUserText(v.Messages)
}

// openAIUserText reads the newest user text of a chat completions body.
func openAIUserText(messages json.RawMessage) string {
	var msgs []anthropicMsg // same role and content shape
	if json.Unmarshal(messages, &msgs) != nil {
		return ""
	}
	return newestUserText(msgs)
}

// captureWriter keeps the first maxAnswerCapture bytes written through it.
type captureWriter struct {
	http.ResponseWriter
	buf bytes.Buffer
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if room := maxAnswerCapture - c.buf.Len(); room > 0 {
		c.buf.Write(b[:min(len(b), room)])
	}
	return c.ResponseWriter.Write(b)
}

func (c *captureWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// capturing wraps w when the prompt log is on; nil otherwise.
func capturing(ctx context.Context, w http.ResponseWriter) *captureWriter {
	if !promptCapture(ctx) {
		return nil
	}
	return &captureWriter{ResponseWriter: w}
}

// noteAnswer keeps the answer text read from what was captured.
func (m *Meta) noteAnswer(c *captureWriter, read func([]byte) string) {
	if c == nil {
		return
	}
	m.Answer = clip(read(c.buf.Bytes()), accounts.PromptLogCapture)
}

// sseData calls fn with the data of each event of an event stream.
func sseData(raw []byte, fn func([]byte)) {
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64<<10), maxAnswerCapture)
	for sc.Scan() {
		if data, ok := bytes.CutPrefix(bytes.TrimSpace(sc.Bytes()), []byte("data:")); ok {
			fn(bytes.TrimSpace(data))
		}
	}
}

// anthropicAnswerText reads the text of an Anthropic answer, streamed or
// whole; tool calls are left out.
func anthropicAnswerText(raw []byte) string {
	var b strings.Builder
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		var v struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		_ = json.Unmarshal(raw, &v)
		for _, c := range v.Content {
			if c.Type == "text" {
				b.WriteString(c.Text)
			}
		}
		return b.String()
	}
	sseData(raw, func(data []byte) {
		var e struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal(data, &e) == nil && e.Type == "content_block_delta" && e.Delta.Type == "text_delta" {
			b.WriteString(e.Delta.Text)
		}
	})
	return b.String()
}

// openAIAnswerText reads the text of a chat completions answer, streamed or
// whole; tool calls are left out.
func openAIAnswerText(raw []byte) string {
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		var v struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(raw, &v) != nil || len(v.Choices) == 0 {
			return ""
		}
		return v.Choices[0].Message.Content
	}
	var b strings.Builder
	sseData(raw, func(data []byte) {
		var v struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal(data, &v) == nil && len(v.Choices) > 0 {
			b.WriteString(v.Choices[0].Delta.Content)
		}
	})
	return b.String()
}
