// Package gateway is the pass-through between an agent and a model API. It
// holds each request in a queue, decides where it goes, and streams the answer
// back untouched. Tool payloads, system prompts and code always pass byte for
// byte; only prose a person typed may be shortened on the cloud leg, when the
// organisation turned that on (shorten.go). It never stores or logs the
// caller's credential, and never records prompt text.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/logx"
)

const (
	// MaxBody bounds a request; long agent conversations are large but not unbounded.
	MaxBody = 32 << 20
	// KeyHeader carries the CodeSynapse gateway key, so Authorization and
	// x-api-key stay the caller's own credential for the cloud leg.
	KeyHeader = "X-CodeSynapse-Key"
)

// AnthropicUpstream is the only cloud address the gateway forwards to.
const AnthropicUpstream = "https://api.anthropic.com"

// forwardRequest headers: everything else (cookies, CodeSynapse headers,
// forwarding headers) stays behind.
var forwardRequest = []string{
	"Content-Type", "Accept", "Authorization", "X-Api-Key",
	"Anthropic-Version", "Anthropic-Beta", "User-Agent",
}

// forwardResponse headers returned to the agent.
var forwardResponse = []string{
	"Content-Type", "Retry-After", "Request-Id", "Anthropic-Ratelimit-Requests-Remaining",
	"Anthropic-Ratelimit-Tokens-Remaining", "Anthropic-Organization-Id",
}

// Meta is what the gateway may record about a call: never text.
type Meta struct {
	Path   string
	Model  string
	Stream bool
	Bytes  int
	Tools  bool
	// Route is where the call was answered: local, cloud, or rejected when the
	// gateway refused it before any model saw it.
	Route string
	// Out is the response size in bytes.
	Out int
	// InTokens and OutTokens are what the local model reported for a local
	// answer; Metered says it reported any. CachedTokens is the part of
	// InTokens its prompt cache served, zero when the runtime reports none
	// (Ollama never does).
	InTokens, OutTokens, CachedTokens int
	Metered                           bool
	// Fallthrough is why a request offered to the local model went on to
	// the cloud: one of the Fall* reasons, empty when the local model
	// answered or was never asked.
	Fallthrough string
	// Shortened says the local model shortened prose of a cloud request;
	// Removed is the bytes taken out of this request and Saved the part a
	// rewrite made for this request removed (Shortened in shorten.go).
	Shortened      bool
	Removed, Saved int
	// Asked says the request reached the local model's queue; QueueWait is
	// how long it waited there and LocalAnswer how long the local model
	// then took to answer or to fail.
	Asked                  bool
	QueueWait, LocalAnswer time.Duration
	// FirstByte is the time from the request to the first byte of the
	// answer, zero when nothing was written.
	FirstByte time.Duration
}

// countWriter counts the bytes of the response and notes when the first
// one was written.
type countWriter struct {
	http.ResponseWriter
	n     int
	start time.Time
	first time.Duration
}

func (c *countWriter) Write(b []byte) (int, error) {
	if c.first == 0 && len(b) > 0 {
		c.first = max(time.Since(c.start), time.Nanosecond)
	}
	n, err := c.ResponseWriter.Write(b)
	c.n += n
	return n, err
}

func (c *countWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Anthropic forwards Anthropic-compatible requests.
type Anthropic struct {
	// Upstream is the base URL; empty means AnthropicUpstream. Tests point it
	// at a local server.
	Upstream string
	// Client must not follow redirects.
	Client *http.Client
	// Local, when set and returning a model, answers the requests on
	// /v1/messages it accepts (see Local.Eligible); the rest, and any it
	// cannot take in time or answer whole, go to the cloud.
	Local func() *Local
	// Shorten, when set, shortens the user's typed prose of /v1/messages
	// requests that go to the cloud, for organisations that turned it on
	// (WithShortenPrompts).
	Shorten *Shortener
	// Done is called once per request with what happened; it must not retain
	// anything but the values it is given.
	Done func(ctx context.Context, m Meta, status int, took time.Duration)
}

// NewAnthropic returns a gateway whose client never follows redirects.
func NewAnthropic() *Anthropic {
	return &Anthropic{Client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// Handler serves POST /v1/messages and POST /v1/messages/count_tokens.
func (g *Anthropic) Handler(upstreamPath string) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		start := time.Now()
		w := &countWriter{ResponseWriter: rw, start: start}
		done := func(ctx context.Context, m Meta, status int) {
			if g.Done != nil {
				m.Out, m.FirstByte = w.n, w.first
				g.Done(ctx, m, status, time.Since(start))
			}
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
		if err != nil {
			problem(w, http.StatusRequestEntityTooLarge, "request body too large")
			done(r.Context(), Meta{Path: upstreamPath, Route: "rejected"}, http.StatusRequestEntityTooLarge)
			return
		}
		if holdsGatewayKey(r) {
			// A CodeSynapse key in the model-credential headers would go to
			// the cloud; refuse before anything is sent.
			problem(w, http.StatusBadRequest, "that is a CodeSynapse key; send it in the "+KeyHeader+" header and your Anthropic credential in Authorization or x-api-key")
			return
		}
		meta := inspect(upstreamPath, body)
		local := g.local()
		if local == nil && g.Local != nil && upstreamPath == "/v1/messages" {
			// A gateway meant to answer locally has no local model right now.
			meta.Fallthrough = FallNoLocalModel
		}
		if local != nil && upstreamPath == "/v1/messages" {
			rec := &statusWriter{ResponseWriter: w}
			u, reason, ok := local.serve(r.Context(), rec, body)
			meta.Asked, meta.QueueWait, meta.LocalAnswer = u.Asked, u.Wait, u.Answer
			if ok {
				meta.Route = "local"
				meta.InTokens, meta.OutTokens, meta.CachedTokens, meta.Metered = u.In, u.Out, u.Cached, u.In+u.Out > 0
				done(r.Context(), meta, http.StatusOK)
				return
			}
			meta.Fallthrough = reason
		}
		meta.Route = "cloud"
		if !hasCredential(r) {
			problem(w, http.StatusUnauthorized, "send your Anthropic credential (Authorization or x-api-key) with the request; the gateway does not store one")
			meta.Route = "rejected"
			done(r.Context(), meta, http.StatusUnauthorized)
			return
		}
		if upstreamPath == "/v1/messages" && g.Shorten != nil && shortenOn(r.Context()) {
			var sh Shortened
			body, sh = g.Shorten.Apply(r.Context(), body)
			meta.Shortened, meta.Removed, meta.Saved = sh.Changed, sh.Removed, sh.New
		}
		status, err := g.forward(r.Context(), w, r, upstreamPath, body)
		if err != nil {
			logx.FromContext(r.Context()).Warn("gateway forward failed", "error", err)
			if status == 0 {
				problem(w, http.StatusBadGateway, "the model API could not be reached")
				status = http.StatusBadGateway
			}
		}
		done(r.Context(), meta, status)
	}
}

func (g *Anthropic) local() *Local {
	if g.Local == nil {
		return nil
	}
	return g.Local()
}

// holdsGatewayKey reports whether a CodeSynapse key sits where the caller's
// own model credential belongs.
func holdsGatewayKey(r *http.Request) bool {
	bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return strings.HasPrefix(bearer, accounts.KeyPrefix) || strings.HasPrefix(r.Header.Get("X-Api-Key"), accounts.KeyPrefix)
}

func hasCredential(r *http.Request) bool {
	return r.Header.Get("X-Api-Key") != "" || strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// inspect reads only the fields that describe the call.
func inspect(path string, body []byte) Meta {
	var v struct {
		Model  string            `json:"model"`
		Stream bool              `json:"stream"`
		Tools  []json.RawMessage `json:"tools"`
	}
	_ = json.Unmarshal(body, &v) // a bad body is the upstream's to refuse
	model := v.Model
	if len(model) > 64 {
		model = model[:64]
	}
	return Meta{Path: path, Model: model, Stream: v.Stream, Bytes: len(body), Tools: len(v.Tools) > 0}
}

// forward sends the request to the upstream and streams the answer. It returns
// the upstream status, or 0 when nothing was written yet.
func (g *Anthropic) forward(ctx context.Context, w http.ResponseWriter, r *http.Request, path string, body []byte) (int, error) {
	base := g.Upstream
	if base == "" {
		base = AnthropicUpstream
	}
	u, err := url.JoinPath(base, path)
	if err != nil {
		return 0, fmt.Errorf("upstream address: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	for _, h := range forwardRequest {
		if v := r.Header.Values(h); len(v) > 0 {
			req.Header[http.CanonicalHeaderKey(h)] = append([]string(nil), v...)
		}
	}
	resp, err := g.Client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("upstream: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read side
	for _, h := range forwardResponse {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set(RouteHeader, "cloud")
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return resp.StatusCode, nil // the agent went away
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return resp.StatusCode, nil
			}
			return resp.StatusCode, fmt.Errorf("stream: %w", rerr)
		}
	}
}

// statusWriter lets Local.serve write through unchanged; it exists so the
// local path never touches the caller's headers before it commits.
type statusWriter struct{ http.ResponseWriter }

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// problem answers in the shape Anthropic clients parse.
func problem(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": "api_error", "message": msg},
	})
}
