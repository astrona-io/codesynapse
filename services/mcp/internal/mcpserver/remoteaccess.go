package mcpserver

import (
	"context"
	"fmt"
	"net/http"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// APIAccess is the accounts.RemoteAccess of the MCP server: it holds no
// OpenFGA key and asks the API (POST /api/v1/internal/access) with the calling
// agent's own key and runtime session, never the installation token. Anything
// but a clean answer refuses (docs/plans/OPENFGA_PLAN.md, phase 6).
type APIAccess struct{ API *APIClient }

// AccessIn implements accounts.RemoteAccess.
func (a APIAccess) AccessIn(ctx context.Context, p accounts.Principal, q accounts.AccessQuery) (accounts.Access, error) {
	c := callerFrom(ctx)
	if a.API == nil || c == nil || c.token == "" || c.UserID != p.User.ID {
		return accounts.Access{}, fmt.Errorf("%w: access is checked with the caller's own key", accounts.ErrForbidden)
	}
	var out accounts.Access
	if err := a.API.send(ctx, c.token, "/api/v1/internal/access", q, http.StatusOK, &out); err != nil {
		return accounts.Access{}, fmt.Errorf("%w: %w", accounts.ErrForbidden, err)
	}
	return out, nil
}
