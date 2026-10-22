package mcpserver

import (
	"context"
	"encoding/json"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/parisnakitakejser/codesynapse/internal/usage"
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
				rec.Record("", usage.Session, client, 0, 0, err != nil)
				return res, err
			case "tools/call", "prompts/get":
				start := time.Now()
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
				failed := err != nil
				if r, ok := res.(*mcp.CallToolResult); ok && r != nil && r.IsError {
					failed = true
				}
				tokens := 0
				if res != nil {
					if b, merr := json.Marshal(res); merr == nil {
						tokens = len(b) / 4
					}
				}
				rec.Record(repo, name, sessionClient(req), time.Since(start), tokens, failed)
				return res, err
			}
			return next(ctx, method, req)
		}
	}
}

func sessionClient(req mcp.Request) string {
	if ss, ok := req.GetSession().(*mcp.ServerSession); ok {
		if p := ss.InitializeParams(); p != nil && p.ClientInfo != nil {
			return p.ClientInfo.Name
		}
	}
	return ""
}
