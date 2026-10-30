package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestEveryRouteIsClassified keeps route registration inside s.route and
// s.account, which classify each pattern for governance-only accounts.
func TestEveryRouteIsClassified(t *testing.T) {
	files, err := os.ReadDir(".")
	require.NoError(t, err)
	calls := regexp.MustCompile(`\.(HandleFunc|Handle)\(`)
	registrations := 0
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(f.Name())
		require.NoError(t, err)
		for _, line := range strings.Split(string(src), "\n") {
			if !calls.MatchString(line) {
				continue
			}
			registrations++
			assert.True(t, strings.Contains(line, "s.mux.HandleFunc(pattern, "), f.Name()+": register routes with s.route or s.account: "+strings.TrimSpace(line))
		}
	}
	assert.Equal(t, registrations, 2, "s.route and s.account")
}

func rootServer(t *testing.T) (*Server, *httptest.Server, *accounts.Service) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	srv := New(ctx, Config{Accounts: acc, ServiceToken: "svc"}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return srv, ts, acc
}

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

// TestRootRefusedEveryBusinessRoute calls every registered business route as
// root, including read-only ones, after giving root a membership and a
// direct repository grant behind the service's back.
func TestRootRefusedEveryBusinessRoute(t *testing.T) {
	srv, ts, acc := rootServer(t)
	ctx := context.Background()
	setup, err := acc.SetupState(ctx)
	require.NoError(t, err)
	org, err := acc.St.OrgByID(ctx, setup.OrgID)
	require.NoError(t, err)
	require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: org.ID, Graph: storetest.Repo, Visibility: accounts.Public, AddedBy: setup.OwnerID, AddedAt: time.Now()}))
	require.NoError(t, acc.St.SetMember(ctx, org.ID, setup.RootUserID, accounts.Owner))
	require.NoError(t, acc.St.WriteTuple(ctx, accounts.Tuple{ObjectType: accounts.TypeRepository, ObjectID: accounts.RepoObject(org.ID, storetest.Repo),
		Relation: accounts.RepoAdmin, SubjectType: accounts.TypeUser, SubjectID: setup.RootUserID, CreatedAt: time.Now()}))
	root := rootClient(t, ts)

	require.True(t, len(srv.classes) > 100, "every route is registered through a classifier")
	for pattern, class := range srv.classes {
		if class == publicRoute {
			continue // no account involved, and logout would end root's session
		}
		method, path, _ := strings.Cut(pattern, " ")
		path = pathParam.ReplaceAllStringFunc(path, func(p string) string {
			if p == "{repo}" || p == "{repo...}" {
				return storetest.Repo
			}
			if p == "{org}" {
				return org.Slug
			}
			return "x"
		})
		body := ""
		if method != "GET" && method != "DELETE" {
			body = "{}"
		}
		var problem map[string]any
		status := root.do(method, path, body, &problem, OrgHeader, org.Slug)
		refused := status == 403 && problem["detail"] == any(accounts.ErrRootAccount.Error())
		switch class {
		case businessRoute:
			assert.True(t, refused, "root must be refused: "+pattern)
		case governanceRoute:
			assert.False(t, refused, "governance stays open: "+pattern)
		}
	}

	var events struct{ Events []accounts.Event }
	require.Equal(t, root.do("GET", "/api/v1/admin/audit?action=root.denied&limit=5", "", &events), 200)
	require.True(t, len(events.Events) > 0, "denials are audited")
	assert.Equal(t, events.Events[0].Outcome, accounts.Failure)
	assert.True(t, strings.HasPrefix(events.Events[0].Target, "GET ") || strings.Contains(events.Events[0].Target, " /api/"), "the route, not the request: "+events.Events[0].Target)
}

func TestRootGovernance(t *testing.T) {
	_, ts, acc := rootServer(t)
	ctx := context.Background()
	acc.SignupOpen = true
	bob := newClient(t, ts)
	var signed struct {
		User accounts.User
		Org  accounts.Org
	}
	require.Equal(t, bob.signUp(`{"email":"bob@example.com","name":"Bob","password":"`+pw+`"}`, &signed), 202)
	root := rootClient(t, ts)

	var me struct {
		Restricted  bool
		Setup       string
		Memberships []accounts.Membership
	}
	require.Equal(t, root.do("GET", "/api/v1/me", "", &me), 200)
	assert.True(t, me.Restricted)
	assert.Equal(t, me.Setup, accounts.SetupComplete)
	assert.Len(t, me.Memberships, 0, "no organisation to switch to")
	assert.Equal(t, root.do("GET", "/api/v1/admin/users", "", nil), 200)
	assert.Equal(t, root.do("GET", "/api/v1/admin/health", "", nil), 200)
	assert.Equal(t, root.do("GET", "/api/v1/stats/series?panel=tenants&range=1h", "", nil), 409, "platform telemetry reaches the unconfigured metrics check")
	assert.Equal(t, root.do("GET", "/api/v1/settings", "", nil), 200)

	// Owner recovery promotes a distinct standard member and gives root
	// nothing.
	carol := newClient(t, ts)
	var carolSigned struct{ User accounts.User }
	require.Equal(t, carol.signUp(`{"email":"carol@example.com","name":"Carol","password":"`+pw+`"}`, &carolSigned), 202)
	invite(t, bob, signed.Org.Slug, carol, "carol@example.com", "viewer")
	assert.Equal(t, root.do("POST", "/api/v1/admin/orgs/"+signed.Org.ID+"/owner", `{"user_id":"`+carolSigned.User.ID+`"}`, nil), 204)
	role, err := acc.St.MemberRole(ctx, signed.Org.ID, carolSigned.User.ID)
	require.NoError(t, err)
	assert.Equal(t, role, accounts.Owner)
	assert.Equal(t, bob.do("POST", "/api/v1/admin/orgs/"+signed.Org.ID+"/owner", `{"user_id":"`+carolSigned.User.ID+`"}`, nil), 403, "platform administration only")
	var events struct{ Events []accounts.Event }
	require.Equal(t, root.do("GET", "/api/v1/admin/audit?action=org.owner.recover", "", &events), 200)
	assert.Len(t, events.Events, 1)
}

// TestSingleTenantHasNoRoot: without accounts there is no root account; the
// installation token is the operator credential and the outer bearer check
// guards everything (README, docs/ROOT_BOOTSTRAP_PLAN.md).
func TestSingleTenantHasNoRoot(t *testing.T) {
	ts, _ := newTestServer(t)
	c := newClient(t, ts)
	assert.Equal(t, c.do("POST", "/api/v1/auth/setup", `{}`, nil), 409, "accounts are off")
}
