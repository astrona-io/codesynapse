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

// Caller identifies the authenticated personal key and its organisation.
// Accounts mode requires a caller on both HTTP and stdio.
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
// accounts.AccessOf narrowed to one explicit organisation, keyed by graph.
type Access interface {
	Authenticate(ctx context.Context, token string) (*Caller, error)
	Denied(ctx context.Context, caller *Caller, tool string) error
	Reach(ctx context.Context, userID, orgID string) (map[string]accounts.Reach, error)
}

type callerKey struct{}

func callerFrom(ctx context.Context) *Caller {
	c, _ := ctx.Value(callerKey{}).(*Caller)
	return c
}

// callerMiddleware puts the caller from the request's token into the
// context every handler receives.
func callerMiddleware(access Access, stdioToken string) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if access != nil {
				token := stdioToken
				if extra := req.GetExtra(); extra != nil && extra.TokenInfo != nil {
					token, _ = extra.TokenInfo.Extra[ExtraToken].(string)
				}
				c, err := access.Authenticate(ctx, token)
				if err != nil || c == nil || c.UserID == "" || c.OrgID == "" || c.KeyID == "" {
					return nil, fmt.Errorf("personal API key required")
				}
				ctx = context.WithValue(ctx, callerKey{}, c)
				ctx = telemetry.WithTenant(ctx, c.OrgID)
			}
			return next(ctx, method, req)
		}
	}
}

// reach returns what the caller reaches; restricted is false when every
// graph is allowed (explicit local single-user mode).
func (h *handlers) reach(ctx context.Context) (map[string]accounts.Reach, bool, error) {
	c := callerFrom(ctx)
	if c == nil {
		if h.access != nil {
			return nil, true, fmt.Errorf("personal API key required")
		}
		return nil, false, nil
	}
	if h.access == nil {
		return map[string]accounts.Reach{}, true, nil // a key without an access source sees nothing
	}
	if r, ok := ctx.Value(reachKey{}).(map[string]accounts.Reach); ok {
		return r, true, nil
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

// canWrite requires repository WRITE permission for memory and reviews.
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

// memRepo keeps memory separate for organisations sharing a code graph.
func (h *handlers) memRepo(ctx context.Context, graph string) string {
	c := callerFrom(ctx)
	if c == nil {
		return graph
	}
	return memory.Namespace(c.OrgID, graph)
}

// reviewOrg is the organisation a review belongs to ("" without a caller).
func reviewOrg(ctx context.Context) string {
	if c := callerFrom(ctx); c != nil {
		return c.OrgID
	}
	return ""
}

// canSeeReview keeps private review content inside the authenticated tenant.
func (h *handlers) canSeeReview(ctx context.Context, repo, org string) error {
	ok, err := h.sees(ctx, repo)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no such review")
	}
	if c := callerFrom(ctx); c != nil && org != c.OrgID {
		return fmt.Errorf("no such review")
	}
	return nil
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
