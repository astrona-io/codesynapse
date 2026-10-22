package mcpserver

import (
	"context"
	"fmt"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/parisnakitakejser/codesynapse/internal/accounts"
	"github.com/parisnakitakejser/codesynapse/internal/memory"
	"github.com/parisnakitakejser/codesynapse/internal/telemetry"
)

// Caller is who a tool call acts for when it came with a personal API key:
// the key's user and organisation. Calls with the MCP token (or over stdio)
// have no caller and see every repository.
type Caller struct {
	UserID string
	Email  string
	OrgID  string
	KeyID  string
	// token is the caller's own key, forwarded to the API for calls it
	// makes on their behalf; it is never logged or returned.
	token string
}

// Token info Extra keys the key verifier fills (see cmd/codesynapse-mcp).
const (
	ExtraOrgID = "org_id"
	ExtraEmail = "email"
	ExtraKeyID = "key_id"
	ExtraToken = "token"
)

// Access says what a key's user reaches in the key's organisation:
// accounts.AccessOf narrowed with Scope (organisation roles, teams,
// repository grants and shares), keyed by graph.
type Access interface {
	Reach(ctx context.Context, userID, orgID string) (map[string]accounts.Reach, error)
}

type callerKey struct{}

func callerFrom(ctx context.Context) *Caller {
	c, _ := ctx.Value(callerKey{}).(*Caller)
	return c
}

// callerMiddleware puts the caller from the request's token into the
// context every handler receives.
func callerMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if extra := req.GetExtra(); extra != nil && extra.TokenInfo != nil {
				ti := extra.TokenInfo
				if org, _ := ti.Extra[ExtraOrgID].(string); org != "" {
					email, _ := ti.Extra[ExtraEmail].(string)
					key, _ := ti.Extra[ExtraKeyID].(string)
					tok, _ := ti.Extra[ExtraToken].(string)
					ctx = context.WithValue(ctx, callerKey{}, &Caller{UserID: ti.UserID, Email: email, OrgID: org, KeyID: key, token: tok})
					ctx = telemetry.WithTenant(ctx, org)
				}
			}
			return next(ctx, method, req)
		}
	}
}

// reach returns what the caller reaches; restricted is false when every
// graph is allowed (the MCP token, stdio).
func (h *handlers) reach(ctx context.Context) (map[string]accounts.Reach, bool, error) {
	c := callerFrom(ctx)
	if c == nil {
		return nil, false, nil
	}
	if h.access == nil {
		return map[string]accounts.Reach{}, true, nil // a key without an access source sees nothing
	}
	r, err := h.access.Reach(ctx, c.UserID, c.OrgID)
	return r, true, err
}

// allowed returns the graphs the caller may use; restricted is false when
// every graph is allowed.
func (h *handlers) allowed(ctx context.Context) (graphs []string, restricted bool, err error) {
	r, restricted, err := h.reach(ctx)
	for g := range r {
		graphs = append(graphs, g)
	}
	slices.Sort(graphs)
	return graphs, restricted, err
}

// canWrite: writing memory or reviews needs editor (organisation members,
// editor grants); viewers and share recipients read only.
func (h *handlers) canWrite(ctx context.Context, graph string) error {
	r, restricted, err := h.reach(ctx)
	if err != nil || !restricted {
		return err
	}
	if !r[graph].Role.AtLeast(accounts.Member) {
		return fmt.Errorf("repository %q is read-only for you: writing memory or reviews needs editor access", graph)
	}
	return nil
}

// sees reports whether the caller may use graph.
func (h *handlers) sees(ctx context.Context, graph string) (bool, error) {
	g, restricted, err := h.allowed(ctx)
	if err != nil || !restricted {
		return err == nil, err
	}
	return slices.Contains(g, graph), nil
}

// memRepo is the memory namespace of graph for the caller: the content
// organisation of their grant (their own, or the one that shared it), so
// organisations sharing a graph keep separate memories.
func (h *handlers) memRepo(ctx context.Context, graph string) string {
	c := callerFrom(ctx)
	if c == nil {
		return graph
	}
	org := c.OrgID
	if r, _, err := h.reach(ctx); err == nil && r[graph].OrgID != "" {
		org = r[graph].OrgID
	}
	return memory.Namespace(org, graph)
}

// reviewOrg is the organisation a review belongs to ("" without a caller).
func reviewOrg(ctx context.Context) string {
	if c := callerFrom(ctx); c != nil {
		return c.OrgID
	}
	return ""
}

// canSeeReview checks a review's repository and organisation. Reviews
// without an organisation (scheduled ones on shared code) are visible to
// every organisation using the repository.
func (h *handlers) canSeeReview(ctx context.Context, repo, org string) error {
	c := callerFrom(ctx)
	if c == nil {
		return nil
	}
	ok, err := h.sees(ctx, repo)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no such review")
	}
	if org == "" || org == c.OrgID {
		return nil
	}
	if r, _, err := h.reach(ctx); err == nil && r[repo].OrgID == org {
		return nil // content follows the repository's grant (shares included)
	}
	return fmt.Errorf("no such review")
}

func callerToken(ctx context.Context) string {
	if c := callerFrom(ctx); c != nil {
		return c.token
	}
	return ""
}

func callerOrg(ctx context.Context) string {
	if c := callerFrom(ctx); c != nil {
		return c.OrgID
	}
	return ""
}
