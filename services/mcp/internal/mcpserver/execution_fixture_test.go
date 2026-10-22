package mcpserver

import (
	"context"
	"fmt"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/parisnakitakejser/codesynapse/internal/accounts"
	"github.com/parisnakitakejser/codesynapse/internal/execution"
	"github.com/parisnakitakejser/codesynapse/internal/store"
)

// Graph-focused unit tests use an explicit registered runtime fixture without
// changing their repository namespace. Accounts integration tests use the real
// registry and personal credentials instead.
type fixtureRuntimeAccess struct{}

func (fixtureRuntimeAccess) Authenticate(_ context.Context, token string) (*Caller, error) {
	if token != "fixture-key" {
		return nil, fmt.Errorf("invalid fixture credential")
	}
	return &Caller{UserID: "fixture-user", OrgID: "org-fixture", KeyID: "fixture-key", token: token}, nil
}
func (fixtureRuntimeAccess) Denied(context.Context, *Caller, string) error { return nil }
func (fixtureRuntimeAccess) Reach(context.Context, string, string) (map[string]accounts.Reach, error) {
	return nil, fmt.Errorf("fixture is not an authorization provider")
}
func (fixtureRuntimeAccess) ResolveExecutionSession(_ context.Context, user, org, session string) (execution.Identity, error) {
	if user != "fixture-user" || org != "org-fixture" || session != "fixture-chat" {
		return execution.Identity{}, fmt.Errorf("unregistered fixture session")
	}
	return execution.Identity{SubjectID: user, TenantID: org, SessionID: session, ParentSessionID: session}, nil
}
func newTestServer(t *testing.T, r store.Reader, version string, options ...Options) *mcp.Server {
	t.Helper()
	var opts Options
	if len(options) > 0 {
		opts = options[0]
	}
	if opts.Access == nil {
		opts.Runtime = NewRuntime(fixtureRuntimeAccess{}, "fixture-key", "fixture-chat", "fixture-chat")
	}
	return New(r, version, opts)
}
