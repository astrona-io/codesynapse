package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/execution"
)

// KeyVerifier accepts personal API keys for the key's user and organisation.
// The installation token cannot authorize repository access in accounts mode.
// Revocation and removed memberships take effect on the next request, since
// every request is checked against the accounts store.
func KeyVerifier(acc *accounts.Service, _ string) auth.TokenVerifier {
	return func(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		p, err := acc.AuthenticateKey(ctx, token)
		if errors.Is(err, accounts.ErrRootAccount) {
			// The caller holds a valid key; tell them why it no longer works.
			if _, auditErr := acc.Record(ctx, accounts.Event{Action: "root.denied", Target: "mcp", Outcome: accounts.Failure, Status: http.StatusUnauthorized}); auditErr != nil {
				return nil, fmt.Errorf("%w: authentication unavailable", auth.ErrInvalidToken)
			}
			return nil, fmt.Errorf("%w: %s", auth.ErrInvalidToken, err.Error())
		}
		if err != nil {
			if _, auditErr := acc.Record(ctx, accounts.Event{Action: "mcp.authentication_failed", Outcome: accounts.Failure, Status: http.StatusUnauthorized}); auditErr != nil {
				return nil, fmt.Errorf("%w: authentication unavailable", auth.ErrInvalidToken)
			}
			// The message reaches the client: say nothing about why.
			return nil, fmt.Errorf("%w: unknown, revoked or expired key", auth.ErrInvalidToken)
		}
		return &auth.TokenInfo{UserID: p.User.ID, Extra: map[string]any{
			ExtraOrgID: p.OrgID, ExtraEmail: p.User.Email, ExtraKeyID: p.KeyID, ExtraToken: token,
		}}, nil
	}
}

// AccountsAccess answers Access from the accounts service: the same
// relationship rules (organisation roles, teams, grants, shares) as the API.
type AccountsAccess struct{ Acc *accounts.Service }

// ResolveExecutionSession rechecks ownership, membership, expiry and revocation.
func (a AccountsAccess) ResolveExecutionSession(ctx context.Context, userID, orgID, sessionID string) (execution.Identity, error) {
	return a.Acc.ResolveExecutionSession(ctx, userID, orgID, sessionID)
}

// Reach implements Access.
func (a AccountsAccess) Reach(ctx context.Context, userID, orgID string) (map[string]accounts.Reach, error) {
	access, err := a.Acc.AccessOf(ctx, userID)
	if err != nil {
		return nil, err
	}
	if _, member := access.Orgs[orgID]; !member {
		return map[string]accounts.Reach{}, nil
	}
	return access.Scope(map[string]bool{orgID: true}), nil
}

// Authenticate checks credentials again for each invocation, including stdio.
func (a AccountsAccess) Authenticate(ctx context.Context, token string) (*Caller, error) {
	p, err := a.Acc.AuthenticateKey(ctx, token)
	if err != nil {
		if _, auditErr := a.Acc.Record(ctx, accounts.Event{Action: "mcp.authentication_failed", Outcome: accounts.Failure, Status: http.StatusUnauthorized}); auditErr != nil {
			return nil, fmt.Errorf("authentication unavailable")
		}
		return nil, fmt.Errorf("personal API key required")
	}
	return &Caller{UserID: p.User.ID, Email: p.User.Email, OrgID: p.OrgID, KeyID: p.KeyID, token: token}, nil
}

// Changed records a successful change made through a tool, without its arguments.
func (a AccountsAccess) Changed(ctx context.Context, c *Caller, action, target string) error {
	e := accounts.Event{Action: action, Target: target, Outcome: accounts.Success, Status: http.StatusOK}
	if c != nil {
		e.UserID, e.ActorEmail, e.OrgID, e.KeyID, e.Via = c.UserID, c.Email, c.OrgID, c.KeyID, "key"
	}
	_, err := a.Acc.Record(ctx, e)
	return err
}

// Request records one MCP request (audit.go) for the caller.
func (a AccountsAccess) Request(ctx context.Context, c *Caller, e accounts.Event) error {
	if c != nil {
		e.UserID, e.ActorEmail, e.OrgID, e.KeyID, e.Via = c.UserID, c.Email, c.OrgID, c.KeyID, "key"
	}
	_, err := a.Acc.Record(ctx, e)
	return err
}

// Denied records authorization failures without storing tool arguments.
func (a AccountsAccess) Denied(ctx context.Context, c *Caller, tool string) error {
	e := accounts.Event{Action: "mcp.access_denied", Target: tool, Outcome: accounts.Failure, Status: http.StatusForbidden}
	if c != nil {
		e.UserID, e.ActorEmail, e.OrgID, e.KeyID, e.Via = c.UserID, c.Email, c.OrgID, c.KeyID, "key"
	}
	_, err := a.Acc.Record(ctx, e)
	return err
}
