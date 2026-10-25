package mcpserver

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/redact"
	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// Every MCP request is recorded in the audit log (docs/SAAS.md, 5c): who
// made it, with which key, from which chat and client, what it asked for and
// how it ended. Results are never kept, only their size. Arguments keep
// identifying values and redacted free text; bulk inputs keep a digest.

// The HTTP layer sets these from the connection; values a client sends are
// dropped first.
const (
	clientAddrHeader = "X-CodeSynapse-Client-Addr"
	requestIDHeader  = "X-CodeSynapse-Request-Id"
)

// RequestAuditor records MCP requests. AccountsAccess implements it; without
// accounts there is no audit log.
type RequestAuditor interface {
	Request(ctx context.Context, caller *Caller, e accounts.Event) error
}

// RequestInfoHTTP gives every request a request ID and the client address
// (X-Forwarded-For only from trusted proxies), for the audit log of the
// key check and, through headers, of the MCP request itself.
func RequestInfoHTTP(trusted []netip.Prefix, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// MCP clients are outside: their traceparent is never continued.
		ctx, span := telemetry.Tracer().Start(r.Context(), "MCP "+r.Method, trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		ri := accounts.RequestInfo{RequestID: cmp.Or(telemetry.TraceID(ctx), newRequestID()), IP: app.ClientAddr(r, trusted), UserAgent: r.UserAgent()}
		r.Header.Set(clientAddrHeader, ri.IP)
		r.Header.Set(requestIDHeader, ri.RequestID)
		// The SDK hands middleware the headers, not this context.
		r.Header.Del("traceparent")
		r.Header.Del("tracestate")
		propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(r.Header))
		next.ServeHTTP(w, r.WithContext(accounts.WithRequestInfo(ctx, ri)))
	})
}

// traceMiddleware continues the HTTP layer's trace with one span per MCP
// method, carrying the tool or prompt name (a fixed set), never arguments.
func traceMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if extra := req.GetExtra(); extra != nil && extra.Header != nil {
				ctx = propagation.TraceContext{}.Extract(ctx, propagation.HeaderCarrier(extra.Header))
			}
			attrs := []attribute.KeyValue{attribute.String("mcp.method", method)}
			switch p := req.GetParams().(type) {
			case *mcp.CallToolParamsRaw:
				attrs = append(attrs, attribute.String("mcp.tool", p.Name))
			case *mcp.GetPromptParams:
				attrs = append(attrs, attribute.String("mcp.prompt", p.Name))
			}
			ctx, span := telemetry.Tracer().Start(ctx, "mcp "+method, trace.WithAttributes(attrs...))
			defer span.End()
			res, err := next(ctx, method, req)
			if r, ok := res.(*mcp.CallToolResult); err != nil || (ok && r != nil && r.IsError) {
				span.SetStatus(codes.Error, "failed")
			}
			return res, err
		}
	}
}

func newRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b) //nolint:errcheck // crypto/rand does not fail on supported platforms
	return hex.EncodeToString(b)
}

// auditMiddleware records each request after it ran. When the entry cannot
// be written the result is withheld: nothing leaves without a trace. Tools
// that change something also audit before the change (Changed).
func (h *handlers) auditMiddleware() mcp.Middleware {
	aud, ok := h.access.(RequestAuditor)
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		if !ok {
			return next
		}
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "ping" || strings.HasPrefix(method, "notifications/") {
				return next(ctx, method, req)
			}
			start := time.Now()
			resolved := repositoryHolder(ctx)
			ctx = context.WithValue(ctx, usageRepositoryKey{}, resolved)
			res, err := next(ctx, method, req)
			m := describeRequest(method, req)
			if m.Repo == "" {
				// The tool omitted repo and the handler picked the only one the
				// caller may use: the entry names it, so sessions show where they ran.
				resolved.mu.Lock()
				m.Repo = resolved.name
				resolved.mu.Unlock()
			}
			m.DurationMS = time.Since(start).Milliseconds()
			if id, ok := execution.From(ctx); ok {
				m.SessionID = id.SessionID
			}
			status := http.StatusOK
			if err != nil {
				status, m.Error = http.StatusInternalServerError, errorLine(err.Error())
			} else if r, ok := res.(*mcp.CallToolResult); ok && r != nil && r.IsError {
				status, m.Error = http.StatusBadRequest, toolError(r)
			}
			if res != nil {
				if b, merr := json.Marshal(res); merr == nil {
					m.Bytes = int64(len(b))
				}
			}
			outcome := accounts.Success
			if status != http.StatusOK {
				outcome = accounts.Failure
			}
			e := accounts.Event{Action: "mcp." + strings.ReplaceAll(method, "/", "."), Target: cmp.Or(m.Name, m.Repo),
				Outcome: outcome, Status: status, MCP: &m}
			if aerr := aud.Request(withHeaderInfo(ctx, req), callerFrom(ctx), e); aerr != nil {
				return nil, fmt.Errorf("the audit log is unavailable, so the result is withheld")
			}
			return res, err
		}
	}
}

// withHeaderInfo carries the HTTP layer's request ID, client address and
// user agent into the audit entry; stdio requests get an ID of their own.
func withHeaderInfo(ctx context.Context, req mcp.Request) context.Context {
	ri := accounts.RequestInfoFrom(ctx)
	if extra := req.GetExtra(); extra != nil && extra.Header != nil {
		ri.RequestID = cmp.Or(extra.Header.Get(requestIDHeader), ri.RequestID)
		ri.IP = cmp.Or(extra.Header.Get(clientAddrHeader), ri.IP)
		ri.UserAgent = cmp.Or(extra.Header.Get("User-Agent"), ri.UserAgent)
	}
	if ri.RequestID == "" {
		ri.RequestID = newRequestID()
	}
	return accounts.WithRequestInfo(ctx, ri)
}

func describeRequest(method string, req mcp.Request) accounts.MCPRequest {
	m := accounts.MCPRequest{Method: method}
	if ss, ok := req.GetSession().(*mcp.ServerSession); ok && ss != nil {
		m.TransportID = ss.ID()
	}
	// The client names itself in initialize (session protocol) or in each
	// request's _meta (sessionless protocol); the SDK reads both.
	if r, ok := req.(interface{ ClientInfo() *mcp.Implementation }); ok {
		if ci := r.ClientInfo(); ci != nil {
			m.Client = strings.TrimSpace(ci.Name + " " + ci.Version)
		}
	}
	switch p := req.GetParams().(type) {
	case *mcp.CallToolParamsRaw:
		m.Name = p.Name
		var args map[string]any
		if json.Unmarshal(p.Arguments, &args) == nil {
			m.Args, m.ArgsDigest = summarizeArgs(args)
			m.Repo, _ = args["repo"].(string)
		} else if len(p.Arguments) > 0 {
			m.ArgsDigest = digest(p.Arguments)
		}
	case *mcp.GetPromptParams:
		m.Name = p.Name
		args := make(map[string]any, len(p.Arguments))
		for k, v := range p.Arguments {
			args[k] = v
		}
		m.Args, m.ArgsDigest = summarizeArgs(args)
		m.Repo = p.Arguments["repo"]
	case *mcp.ReadResourceParams:
		m.Name = p.URI
	}
	return m
}

// bulkArgs carry code or conversations: never kept, only digested.
var bulkArgs = map[string]bool{
	"diff": true, "patch": true, "code": true, "content": true, "body": true, "text": true,
	"turns": true, "messages": true, "triples": true, "comments": true,
}

// maxArgValue is the longest encoded value kept; longer ones are digested.
const maxArgValue = accounts.MaxMCPArgValue

// summarizeArgs keeps each argument's value (redacted, short) or, for bulk
// and long values, its digest and size. The second result digests the whole
// argument object in canonical form (sorted keys).
func summarizeArgs(args map[string]any) (map[string]string, string) {
	if len(args) == 0 {
		return nil, ""
	}
	out := make(map[string]string, len(args))
	for k, v := range args {
		var text string
		switch x := v.(type) {
		case string:
			text = x
		default:
			b, err := json.Marshal(x)
			if err != nil {
				continue
			}
			text = string(b)
		}
		if bulkArgs[strings.ToLower(k)] || len(text) > maxArgValue {
			out[k] = digest([]byte(text))
			continue
		}
		out[k] = redact.Free(text)
	}
	canon, err := json.Marshal(args) // maps marshal with sorted keys
	if err != nil {
		return out, ""
	}
	return out, digest(canon)
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("sha256:%s (%d bytes)", hex.EncodeToString(sum[:]), len(b))
}

func errorLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return redact.Free(s)
}

func toolError(r *mcp.CallToolResult) string {
	for _, c := range r.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			return errorLine(t.Text)
		}
	}
	return "tool error"
}
