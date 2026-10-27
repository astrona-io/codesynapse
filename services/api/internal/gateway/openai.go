package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
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
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		l := o.local()
		if l == nil {
			openAIProblem(w, http.StatusServiceUnavailable, "no local model is set up for the gateway")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
		if err != nil {
			openAIProblem(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		var req map[string]json.RawMessage
		if json.Unmarshal(body, &req) != nil {
			openAIProblem(w, http.StatusBadRequest, "the body is not a JSON object")
			return
		}
		meta := Meta{Path: "/v1/chat/completions", Bytes: len(body), Route: "local"}
		_ = json.Unmarshal(req["stream"], &meta.Stream)
		meta.Tools = len(req["tools"]) > 2
		// The agent names whatever model it has configured; the local one answers.
		model, _ := json.Marshal(l.Model)
		req["model"] = model
		out, err := json.Marshal(req)
		if err != nil {
			openAIProblem(w, http.StatusBadRequest, "the body could not be read")
			return
		}
		status := o.forward(r.Context(), w, l, out)
		if o.Done != nil {
			o.Done(r.Context(), meta, status, time.Since(start))
		}
	}
}

func (o *OpenAI) local() *Local {
	if o.Local == nil {
		return nil
	}
	return o.Local()
}

func (o *OpenAI) forward(ctx context.Context, w http.ResponseWriter, l *Local, body []byte) int {
	release := func() {}
	if l.Acquire != nil {
		var err error
		if release, err = l.Acquire(ctx); err != nil {
			return 0 // the agent gave up while waiting
		}
	}
	defer release()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(l.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		openAIProblem(w, http.StatusBadGateway, "the local model could not be reached")
		return http.StatusBadGateway
	}
	req.Header.Set("Content-Type", "application/json")
	if l.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+l.APIKey)
	}
	client := l.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		logx.FromContext(ctx).Warn("gateway local model failed", "error", err)
		openAIProblem(w, http.StatusBadGateway, "the local model could not be reached")
		return http.StatusBadGateway
	}
	defer resp.Body.Close() //nolint:errcheck // read side
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set(RouteHeader, "local")
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return resp.StatusCode
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				logx.FromContext(ctx).Warn("gateway local stream ended early", "error", rerr)
			}
			return resp.StatusCode
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
