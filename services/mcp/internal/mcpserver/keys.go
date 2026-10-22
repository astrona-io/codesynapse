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

// OrgAccess reads an organisation's repositories from the accounts store.
type OrgAccess struct{ St accounts.Store }

// OrgGraphs implements Access.
func (a OrgAccess) OrgGraphs(ctx context.Context, orgID string) ([]string, error) {
	repos, err := a.St.OrgRepositories(ctx, orgID)
	if err != nil {
		return nil, err
	}
	graphs := make([]string, 0, len(repos))
	for _, r := range repos {
		graphs = append(graphs, r.Graph)
	}
	return graphs, nil
}
