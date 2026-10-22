package mcpserver

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/parisnakitakejser/codesynapse/internal/accounts"
)

// KeyVerifier accepts the installation's MCP token (no caller: every
// repository) and personal API keys (the key's user and organisation).
// Revocation and removed memberships take effect on the next request, since
// every request is checked against the accounts store.
func KeyVerifier(acc *accounts.Service, serviceToken string) auth.TokenVerifier {
	return func(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		if serviceToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(serviceToken)) == 1 {
			return &auth.TokenInfo{UserID: "service"}, nil
		}
		p, err := acc.AuthenticateKey(ctx, token)
		if err != nil {
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

// Reach implements Access.
func (a AccountsAccess) Reach(ctx context.Context, userID, orgID string) (map[string]accounts.Reach, error) {
	access, err := a.Acc.AccessOf(ctx, userID)
	if err != nil {
		return nil, err
	}
	if _, member := access.Orgs[orgID]; !member {
		u, err := a.Acc.St.UserByID(ctx, userID)
		if err != nil || !u.PlatformAdmin {
			return map[string]accounts.Reach{}, err
		}
		// A platform admin's key acts as an owner of the organisation.
		repos, err := a.Acc.St.OrgRepositories(ctx, orgID)
		if err != nil {
			return nil, err
		}
		for _, r := range repos {
			if r.Usable() {
				access.Repos = append(access.Repos, accounts.RepoAccess{OrgID: orgID, Graph: r.Graph, Role: accounts.Admin})
			}
		}
		access.Orgs[orgID] = accounts.Owner
	}
	return access.Scope(map[string]bool{orgID: true}), nil
}
