package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestPlatformContentIsolation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	acc := accounts.NewService(acctmem.New(), true)
	root := bootstrapAccounts(t, acc)
	srv := New(ctx, Config{Accounts: acc, ServiceToken: "svc"}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	admin, owner := newClient(t, ts), newClient(t, ts)
	var adminUser struct{ User accounts.User }
	require.Equal(t, admin.signUp(`{"email":"admin@example.com","name":"Admin","password":"`+pw+`"}`, &adminUser), 202)
	// A standard account with platform administration keeps business work
	// but sees no tenant content without membership.
	require.NoError(t, acc.SetPlatformAdmin(ctx, root, adminUser.User.ID, true))
	var signed struct {
		User accounts.User
		Org  accounts.Org
	}
	require.Equal(t, owner.signUp(`{"email":"owner@example.com","name":"Owner","password":"`+pw+`"}`, &signed), 202)
	require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: signed.Org.ID, Graph: storetest.Repo, AddedBy: signed.User.ID, AddedAt: time.Now()}))
	reportPath := "/api/v1/accounts/orgs/" + signed.Org.Slug + "/access-changes"
	var report accounts.AccessChange
	require.Equal(t, owner.do("GET", reportPath, "", &report), 200)
	assert.Equal(t, report.OrgID, signed.Org.ID)
	assert.Equal(t, report.RepositoryCount, 1)
	assert.Equal(t, admin.do("GET", reportPath, "", nil), 404, "platform status does not reveal tenant migration data")
	var key struct{ Key string }
	require.Equal(t, owner.do("POST", "/api/v1/me/keys", `{"org":"`+signed.Org.Slug+`","name":"report-agent"}`, &key), 201)
	keyClient := newClient(t, ts)
	keyClient.auth = "Bearer " + key.Key
	assert.Equal(t, keyClient.do("GET", reportPath, "", nil), 403, "the report requires an owner session")
	assert.Equal(t, keyClient.do("POST", "/api/v1/repos", `{}`, nil), 403, "keys cannot create an initial administrator grant")
	assert.Equal(t, keyClient.do("POST", "/api/v1/accounts/orgs/"+signed.Org.Slug+"/repos", `{"graph":"`+storetest.Repo+`"}`, nil), 403, "keys cannot attach a graph and grant access")
	assert.Equal(t, owner.do("POST", "/api/v1/repos", `{}`, nil, OrgHeader, signed.Org.Slug, execution.SessionHeader, "unregistered", execution.ParentSessionHeader, "unregistered"), 403,
		"repository registration requires a registered runtime session before side effects")
	assert.Equal(t, owner.do("POST", "/api/v1/repos", `{}`, nil, OrgHeader, signed.Org.Slug), 501, "a signed-in browser uses its dashboard binding")
	require.Equal(t, owner.do("POST", "/api/v1/execution/sessions", `{"session_id":"owner-registration"}`, nil, OrgHeader, signed.Org.Slug), 201)
	headers := append([]string{OrgHeader, signed.Org.Slug}, runtimeHeaders("owner-registration")...)
	assert.Equal(t, owner.do("POST", "/api/v1/repos", `{}`, nil, headers...), 501, "registered owner reaches the workspace configuration check")
	assert.Equal(t, owner.do("POST", "/api/v1/accounts/orgs/"+signed.Org.Slug+"/repos", `{}`, nil, execution.SessionHeader, "unregistered", execution.ParentSessionHeader, "unregistered"), 403,
		"attachment also requires a registered runtime session")
	foreign := model.NewGraph()
	secretID := model.NodeID("private", model.KindFunction, "Secret")
	foreign.AddNode(model.Node{ID: secretID, Repo: "private", Kind: model.KindFunction, Name: "Secret", Summary: "Hidden source summary"})
	require.NoError(t, st.ReplaceRepo(ctx, "private", foreign))
	for _, endpoint := range []string{"symbol", "graph", "graph/expand"} {
		t.Run("foreign node "+endpoint, func(t *testing.T) {
			assert.Equal(t, owner.do("GET", "/api/v1/repos/"+storetest.Repo+"/"+endpoint+"?id="+url.QueryEscape(secretID), "", nil), 404)
		})
	}
	service := newClient(t, ts)
	service.auth = "Bearer svc"
	for name, c := range map[string]*struct {
		*client
		health int
	}{"platform": {admin, 200}, "service": {service, 403}} {
		t.Run(name, func(t *testing.T) {
			var repos struct{ Repos []struct{ Name string } }
			require.Equal(t, c.do("GET", "/api/v1/repos", "", &repos), 200)
			assert.Len(t, repos.Repos, 0)
			assert.Equal(t, c.do("GET", "/api/v1/repos/"+storetest.Repo+"/overview", "", nil), 404)
			assert.Equal(t, c.do("GET", "/api/v1/repos", "", nil, OrgHeader, signed.Org.Slug), 404)
			assert.Equal(t, c.do("POST", "/api/v1/ingest", `{}`, nil), 403)
			assert.Equal(t, c.do("GET", "/api/v1/admin/health", "", nil), c.health, "platform reads need an administrator's session")
		})
	}
	require.Equal(t, owner.do("PUT", "/api/v1/accounts/orgs/"+signed.Org.Slug+"/members", `{"email":"admin@example.com","role":"viewer"}`, nil), 200)
	assert.Equal(t, admin.do("GET", reportPath, "", nil), 404, "membership alone does not permit the report")
	require.Equal(t, owner.do("PUT", "/api/v1/accounts/orgs/"+signed.Org.Slug+"/repo-policy", `{"default_repo_access":"READ_ONLY"}`, nil), 200)
	assert.Equal(t, admin.do("GET", "/api/v1/repos/"+storetest.Repo+"/overview", "", nil, OrgHeader, signed.Org.Slug), 200)
	assert.Equal(t, admin.do("POST", "/api/v1/repos/"+storetest.Repo+"/sync", "", nil, OrgHeader, signed.Org.Slug), 403)
	assert.Equal(t, admin.do("DELETE", "/api/v1/repos/"+storetest.Repo+"/memory", "", nil, OrgHeader, signed.Org.Slug), 403)
}

func TestQueuedJobRechecksSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	user, org, err := acc.SignUp(ctx, "actor@example.com", "Actor", pw)
	require.NoError(t, err)
	require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: org.ID, Graph: "repo", AddedBy: user.ID, AddedAt: time.Now()}))
	// Resolve the principal through authentication so revocation checks use
	// the same hashed session identity as HTTP requests.
	token, _, err := acc.Login(ctx, "actor@example.com", pw, "test")
	require.NoError(t, err)
	p, err := acc.Authenticate(ctx, token)
	require.NoError(t, err)
	srv := &Server{cfg: Config{Accounts: acc}}
	jobCtx := context.WithValue(ctx, principalKey{}, p)
	jobCtx = context.WithValue(jobCtx, scopeKey{}, scope{reach: map[string]accounts.Reach{"repo": {OrgID: org.ID, Role: accounts.Admin}}})
	id, err := acc.RegisterExecutionSession(ctx, user.ID, org.ID, "queued-job-runtime")
	require.NoError(t, err)
	jobCtx, err = execution.With(jobCtx, id)
	require.NoError(t, err)
	_, authorize := srv.jobAuthorization(jobCtx, "repo", accounts.Admin)
	require.NoError(t, authorize(jobCtx))
	jobs := NewJobs(ctx, memory.New(), slog.New(slog.NewTextHandler(io.Discard, nil)), 0, false, false)
	jobs.sem <- struct{}{}
	done := make(chan error, 1)
	_, err = jobs.Start("repo", "", JobOptions{OrgID: org.ID, Identity: id, Authorize: authorize,
		Run: func(context.Context) (string, error) {
			t.Error("revoked job executed")
			return "", nil
		}, Done: func(err error) { done <- err }})
	require.NoError(t, err)
	require.NoError(t, acc.Logout(ctx, token))
	assert.Error(t, authorize(jobCtx), "queued work loses permission when its session ends")
	<-jobs.sem
	select {
	case err := <-done:
		assert.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("queued job did not finish")
	}
}

func TestMalformedRepositoryPolicyDoesNotBlockOwnerRecovery(t *testing.T) {
	ts, acc := accountsServer(t)
	acc.SignupOpen = true
	owner := newClient(t, ts)
	var signed struct {
		User accounts.User
		Org  accounts.Org
	}
	require.Equal(t, owner.signUp(`{"email":"owner@example.com","name":"Owner","password":"`+pw+`"}`, &signed), 202)
	ctx := context.Background()
	require.NoError(t, acc.SetPlatformAdmin(ctx, rootSession(t, acc), signed.User.ID, true))
	require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: signed.Org.ID, Graph: storetest.Repo, AddedBy: signed.User.ID, AddedAt: time.Now()}))
	object := accounts.RepoObject(signed.Org.ID, storetest.Repo)
	_, err := acc.St.DeleteTuples(ctx, accounts.TupleFilter{ObjectType: accounts.TypeRepository, ObjectID: object})
	require.NoError(t, err)
	// Simulate an invalid legacy grant preserved by a policy migration.
	require.NoError(t, acc.St.WriteTuple(ctx, accounts.Tuple{ObjectType: accounts.TypeRepository, ObjectID: object,
		Relation: "invalid", SubjectType: accounts.TypeUser, SubjectID: signed.User.ID, CreatedAt: time.Now()}))
	repoPath := "/api/v1/repos/" + storetest.Repo + "/overview"
	assert.Equal(t, owner.do("GET", repoPath, "", nil), 400, "malformed content policy fails closed")
	assert.Equal(t, owner.do("GET", "/api/v1/settings", "", nil), 200, "platform controls do not resolve content grants")
	assert.Equal(t, owner.do("PUT", "/api/v1/ai/routing", `{}`, nil), 409, "platform authorization reaches the unconfigured provider check")
	orgPath := "/api/v1/accounts/orgs/" + signed.Org.Slug
	assert.Equal(t, owner.do("PUT", orgPath+"/repo-policy", `{"default_repo_access":"NONE"}`, nil), 200)
	require.Equal(t, owner.do("POST", orgPath+"/repos/"+storetest.Repo+"/recover-admin", `{"user":"owner@example.com"}`, nil), 204)
	assert.Equal(t, owner.do("GET", repoPath, "", nil), 200, "explicit recovery replaces the malformed direct grant")
}
