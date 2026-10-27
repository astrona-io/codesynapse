package mcpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/astrona-io/codesynapse/internal/logx"
	"github.com/astrona-io/codesynapse/internal/usage"
)

// usageMiddleware counts sessions, tool calls and prompt requests. Only the
// repo argument is read; other arguments and the results are measured, not
// kept.
func usageMiddleware(rec *usage.Recorder) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			// A client connecting: "initialize" in the session protocol,
			// "server/discover" in the sessionless one (SEP-2575).
			case "initialize", "server/discover":
				res, err := next(ctx, method, req)
				client := sessionClient(req)
				if p, ok := req.GetParams().(*mcp.InitializeParams); ok && p.ClientInfo != nil {
					client = p.ClientInfo.Name
				}
				rec.Record(callerOrg(ctx), "", usage.Session, client, 0, 0, err != nil)
				return res, err
			case "tools/call", "prompts/get":
				start := time.Now()
				resolved := repositoryHolder(ctx)
				ctx = context.WithValue(ctx, usageRepositoryKey{}, resolved)
				res, err := next(ctx, method, req)
				name, repo := "", ""
				switch p := req.GetParams().(type) {
				case *mcp.CallToolParamsRaw:
					name = p.Name
					var args struct {
						Repo string `json:"repo"`
					}
					_ = json.Unmarshal(p.Arguments, &args) //nolint:errcheck // a missing or odd repo counts as the default
					repo = args.Repo
				case *mcp.GetPromptParams:
					name, repo = "prompt:"+p.Name, p.Arguments["repo"]
				}
				ambiguous := false
				if repo == "" {
					resolved.mu.Lock()
					repo = resolved.name
					ambiguous = resolved.ambiguous
					resolved.mu.Unlock()
				}
				failed := err != nil
				if r, ok := res.(*mcp.CallToolResult); ok && r != nil && r.IsError {
					failed = true
				}
				if repo == "" {
					logUnattributed(ctx, name, sessionClient(req), failed, ambiguous)
				}
				tokens := 0
				if res != nil {
					if b, merr := json.Marshal(res); merr == nil {
						tokens = len(b) / 4
					}
				}
				rec.Record(callerOrg(ctx), repo, name, sessionClient(req), time.Since(start), tokens, failed)
				return res, err
			}
			return next(ctx, method, req)
		}
	}
}

// logUnattributed explains a call that lands in the "(no repo argument)" usage
// row. Tools that list or catalogue are repository-less by design; any other
// tool ending here never resolved a repository, which is a bug in the caller's
// arguments or in our resolution, so it is logged to find out which.
func logUnattributed(ctx context.Context, name, client string, failed, ambiguous bool) {
	if p, ok := toolPolicies[name]; ok && (p.target == "list" || p.target == "catalog") {
		return
	}
	logx.FromContext(ctx).Warn("mcp call without a resolved repository",
		slog.String("tool", name),
		slog.String("client", client),
		slog.Bool("failed", failed),
		slog.Bool("ambiguous", ambiguous))
}

type usageRepositoryKey struct{}

// repositoryHolder returns the holder an outer middleware already put in ctx,
// so usage and audit see the same resolution whichever runs outside.
func repositoryHolder(ctx context.Context) *usageRepository {
	if r, ok := ctx.Value(usageRepositoryKey{}).(*usageRepository); ok {
		return r
	}
	return &usageRepository{}
}

type usageRepository struct {
	mu        sync.Mutex
	name      string
	ambiguous bool
}

// Record the handler's authorised resolution, without another graph query or
// guessing that a repository-wide tool belongs to the last repository used.
func noteUsageRepository(ctx context.Context, repo string) {
	if r, ok := ctx.Value(usageRepositoryKey{}).(*usageRepository); ok {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.ambiguous {
			return
		}
		if r.name != "" && r.name != repo {
			r.name, r.ambiguous = "", true
			return
		}
		r.name = repo
	}
}

// clientNameHeader is set by the CodeSynapse bridge from the client's
// initialize message (cmd/codesynapse/bridge.go).
const clientNameHeader = "X-CodeSynapse-Client"

func sessionClient(req mcp.Request) string {
	// The SDK reads per-request metadata for sessionless clients and falls
	// back to initialize for clients using the session protocol.
	if r, ok := req.(interface{ ClientInfo() *mcp.Implementation }); ok {
		if ci := r.ClientInfo(); ci != nil && ci.Name != "" {
			return ci.Name
		}
	}
	// A bridge that carries the name itself; usage cleans and caps it like
	// any client name, so it cannot mint series.
	if extra := req.GetExtra(); extra != nil && extra.Header != nil {
		return extra.Header.Get(clientNameHeader)
	}
	return ""
}
