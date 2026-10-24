package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/astrona-io/codesynapse/internal/agentctx"
)

type attackSurfaceIn struct {
	RepoArg
	Kind      string `json:"kind,omitempty" jsonschema:"http, mcp or cli; empty for all"`
	Reaching  string `json:"reaching,omitempty" jsonschema:"keep entry points that reach this tag: exec, sql, cypher, deserialize, template, unsafe, syscall, filesystem, network, env, weak-crypto, insecure-rand, tls; auth keeps those that call authorization"`
	Unguarded bool   `json:"unguarded,omitempty" jsonschema:"keep entry points with no guard at registration and no authorization call in reach"`
	Query     string `json:"query,omitempty" jsonschema:"keep entry points whose name contains this, e.g. /admin"`
	Limit     int    `json:"limit,omitempty" jsonschema:"max entry points, default 50"`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"estimated JSON token budget, bytes/4; 256-20000, default 20000"`
}

func (h *handlers) attackSurface(ctx context.Context, _ *mcp.CallToolRequest, in attackSurfaceIn) (*mcp.CallToolResult, agentctx.AttackSurface, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, agentctx.AttackSurface{}, err
	}
	out, err := agentctx.Service{R: h.svc.R}.AttackSurface(ctx, repo, agentctx.SurfaceOptions{
		Kind: in.Kind, Reaching: in.Reaching, Unguarded: in.Unguarded, Query: in.Query, Limit: in.Limit, MaxTokens: in.MaxTokens,
	})
	return nil, out, err
}
