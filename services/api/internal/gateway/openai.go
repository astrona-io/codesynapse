package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/logx"
)

// OpenAI serves the OpenAI-compatible endpoints for local agents. Every
// request is answered by the local model, never the cloud: the request waits
// its turn in the model's queue for as long as the agent keeps the connection
// open, then the answer streams back untouched.
type OpenAI struct {
	// Local returns the local model, or nil when none opted in.
	Local func() *Local
	Done  func(ctx context.Context, m Meta, status int, took time.Duration)
}

// ChatHandler serves POST /v1/chat/completions.
func (o *OpenAI) ChatHandler() http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		start := time.Now()
		w := &countWriter{ResponseWriter: rw}
		meta := Meta{Path: "/v1/chat/completions", Route: "rejected"}
		done := func(ctx context.Context, status int) {
			if o.Done != nil {
				meta.Out = w.n
				o.Done(ctx, meta, status, time.Since(start))
			}
		}
		l := o.local()
		if l == nil {
			openAIProblem(w, http.StatusServiceUnavailable, "no local model is set up for the gateway")
			done(r.Context(), http.StatusServiceUnavailable)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
		if err != nil {
			openAIProblem(w, http.StatusRequestEntityTooLarge, "request body too large")
			done(r.Context(), http.StatusRequestEntityTooLarge)
			return
		}
		meta.Bytes = len(body)
		var req map[string]json.RawMessage
		if json.Unmarshal(body, &req) != nil {
			openAIProblem(w, http.StatusBadRequest, "the body is not a JSON object")
			done(r.Context(), http.StatusBadRequest)
			return
		}
		_ = json.Unmarshal(req["stream"], &meta.Stream)
		meta.Tools = len(req["tools"]) > 2
		meta.notePrompt(r.Context(), openAIUserText(req["messages"]))
		// The agent names whatever model it has configured; the local one answers.
		model, _ := json.Marshal(l.Model)
		req["model"] = model
		out, err := json.Marshal(req)
		if err != nil {
			openAIProblem(w, http.StatusBadRequest, "the body could not be read")
			done(r.Context(), http.StatusBadRequest)
			return
		}
		var answer http.ResponseWriter = w
		capture := capturing(r.Context(), w)
		if capture != nil {
			answer = capture
		}
		status, tail := o.forward(r.Context(), answer, l, out)
		if status == 0 {
			return // the agent gave up while waiting; nothing was answered
		}
		if status < 400 {
			meta.Route = "local"
			meta.InTokens, meta.OutTokens, meta.CachedTokens = reportedUsage(tail)
			meta.Metered = meta.InTokens+meta.OutTokens > 0
			meta.noteAnswer(capture, openAIAnswerText)
		}
		done(r.Context(), status)
	}
}

var (
	promptTokens     = regexp.MustCompile(`"prompt_tokens"\s*:\s*(\d+)`)
	completionTokens = regexp.MustCompile(`"completion_tokens"\s*:\s*(\d+)`)
	cachedTokens     = regexp.MustCompile(`"cached_tokens"\s*:\s*(\d+)`)
	cacheN           = regexp.MustCompile(`"cache_n"\s*:\s*(\d+)`)
)

// usageTail is how much of the end of an answer is searched for the usage the
// local server reports.
const usageTail = 4 << 10

// reportedUsage reads the last token counts in the end of an answer: the
// prompt and completion tokens and the part of the prompt the runtime's
// cache served (cached_tokens, else llama.cpp's cache_n).
func reportedUsage(tail []byte) (in, out, cached int) {
	last := func(re *regexp.Regexp) int {
		m := re.FindAllSubmatch(tail, -1)
		if len(m) == 0 {
			return 0
		}
		n, _ := strconv.Atoi(string(m[len(m)-1][1]))
		return n
	}
	in, out, cached = last(promptTokens), last(completionTokens), last(cachedTokens)
	if cached == 0 {
		cached = last(cacheN)
	}
	return in, out, clampCached(cached, in)
}

func (o *OpenAI) local() *Local {
	if o.Local == nil {
		return nil
	}
	return o.Local()
}

// forward streams the local answer and returns its status (0 when nothing
// was written) with the end of the answer.
func (o *OpenAI) forward(ctx context.Context, w http.ResponseWriter, l *Local, body []byte) (int, []byte) {
	release := func() {}
	if l.Acquire != nil {
		var err error
		if release, err = l.Acquire(ctx); err != nil {
			return 0, nil // the agent gave up while waiting
		}
	}
	defer release()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(l.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		openAIProblem(w, http.StatusBadGateway, "the local model could not be reached")
		return http.StatusBadGateway, nil
	}
	req.Header.Set("Content-Type", "application/json")
	if l.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+l.APIKey)
	}
	resp, err := l.client().Do(req)
	if err != nil {
		logx.FromContext(ctx).Warn("gateway local model failed", "error", err)
		openAIProblem(w, http.StatusBadGateway, "the local model could not be reached")
		return http.StatusBadGateway, nil
	}
	defer resp.Body.Close() //nolint:errcheck // read side
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set(RouteHeader, "local")
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16<<10)
	var tail []byte
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return resp.StatusCode, tail
			}
			tail = append(tail, buf[:n]...)
			if len(tail) > usageTail {
				tail = append([]byte(nil), tail[len(tail)-usageTail:]...)
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				logx.FromContext(ctx).Warn("gateway local stream ended early", "error", rerr)
			}
			return resp.StatusCode, tail
		}
	}
}

// ModelsHandler serves GET /v1/models: the one local model.
func (o *OpenAI) ModelsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		l := o.local()
		if l == nil {
			openAIProblem(w, http.StatusServiceUnavailable, "no local model is set up for the gateway")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]any{{"id": l.Model, "object": "model", "owned_by": "codesynapse"}},
		})
	}
}

func openAIProblem(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": msg, "type": "gateway_error"}})
}
