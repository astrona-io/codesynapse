package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/reviews"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestTenantIsolation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	other := model.NewGraph()
	other.AddNode(model.Node{ID: model.NodeID("secret", model.KindRepository, "secret"), Kind: model.KindRepository, Repo: "secret", Name: "secret"})
	require.NoError(t, st.ReplaceRepo(ctx, "secret", other))
	rv, err := reviews.Prepare(reviews.Review{Repo: "secret", Title: "T", Comments: []reviews.CommentItem{{File: "a.go", Line: 1, Message: "m"}}}, time.Now())
	require.NoError(t, err)
	require.NoError(t, st.SaveReview(ctx, rv))
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	ts := httptest.NewServer(New(ctx, Config{Accounts: acc, ServiceToken: "svc"}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)

	signup := func(email string) *client {
		c := newClient(t, ts)
		require.Equal(t, c.signUp(`{"email":"`+email+`","name":"`+email+`","password":"`+pw+`"}`, nil), 202)
		return c
	}
	ada := signup("ada@example.com") // platform admin
	bob := signup("bob@example.com")
	carol := signup("carol@example.com")
	var team, side accounts.Org
	require.Equal(t, bob.do("POST", "/api/v1/accounts/orgs", `{"name":"Team"}`, &team), 201)
	require.Equal(t, ada.do("POST", "/api/v1/accounts/orgs", `{"name":"Side"}`, &side), 201)
	require.Equal(t, bob.do("PUT", "/api/v1/accounts/orgs/"+team.Slug+"/members", `{"email":"carol@example.com","role":"viewer"}`, nil), 200)

	// Platform administration cannot attach a graph to another tenant.
	assert.Equal(t, ada.do("POST", "/api/v1/accounts/orgs/"+team.Slug+"/repos", `{"graph":"`+storetest.Repo+`"}`, nil), 404)
	bobUser, err := acc.St.UserByEmail(ctx, "bob@example.com")
	require.NoError(t, err)
	adaUser, err := acc.St.UserByEmail(ctx, "ada@example.com")
	require.NoError(t, err)
	require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: team.ID, Graph: storetest.Repo, AddedBy: bobUser.ID, AddedAt: time.Now()}))
	require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: side.ID, Graph: storetest.Repo, AddedBy: adaUser.ID, AddedAt: time.Now()}))
	require.Equal(t, bob.do("PUT", "/api/v1/accounts/orgs/"+team.Slug+"/repo-policy", `{"default_repo_access":"READ_ONLY"}`, nil), 200)

	var list struct{ Repos []struct{ Name string } }
	require.Equal(t, bob.do("GET", "/api/v1/repos", "", &list), 200)
	require.Len(t, list.Repos, 1)
	assert.Equal(t, list.Repos[0].Name, storetest.Repo, "only the organisation's repositories")
	var ov Overview
	require.Equal(t, carol.do("GET", "/api/v1/overview", "", &ov), 200)
	assert.Equal(t, ov.Totals.Repos, 1, "overview counts visible repositories only")
	var stats Stats
	require.Equal(t, bob.do("GET", "/api/v1/stats", "", &stats), 200)
	assert.Equal(t, stats.Workspace.Repos, 1)

	assert.Equal(t, bob.do("GET", "/api/v1/repos/"+storetest.Repo+"/overview", "", nil), 200)
	assert.Equal(t, bob.do("GET", "/api/v1/repos/secret/overview", "", nil), 404, "other repositories do not exist for you")
	assert.Equal(t, bob.do("GET", "/api/v1/reviews/"+rv.ID, "", nil), 404, "nor their reviews")
	assert.Equal(t, ada.do("GET", "/api/v1/reviews/"+rv.ID, "", nil), 404)
	assert.Equal(t, bob.do("GET", "/api/v1/sources", "", nil), 403, "mounted sources are for platform admins")
	assert.Equal(t, carol.do("PUT", "/api/v1/repos/"+storetest.Repo+"/schedule", `{"interval_minutes":60}`, nil), 403, "viewers cannot change schedules")
	assert.Equal(t, carol.do("DELETE", "/api/v1/repos/"+storetest.Repo, "", nil), 403)
	for _, endpoint := range []string{"identities", "code-records", "identities/reviews"} {
		assert.Equal(t, bob.do("GET", "/api/v1/repos/secret/"+endpoint, "", nil), 404)
		assert.Equal(t, carol.do("GET", "/api/v1/repos/"+storetest.Repo+"/"+endpoint, "", nil), 200)
	}
	assert.Equal(t, carol.do("POST", "/api/v1/repos/"+storetest.Repo+"/code-records", `{}`, nil), 403)
	assert.Equal(t, carol.do("POST", "/api/v1/repos/"+storetest.Repo+"/identities/reviews", `{}`, nil), 403)

	var created struct{ Key string }
	require.Equal(t, carol.do("POST", "/api/v1/me/keys", `{"org":"`+team.Slug+`","name":"agent"}`, &created), 201)
	agent := newClient(t, ts)
	agent.auth = "Bearer " + created.Key
	assert.Equal(t, agent.do("GET", "/api/v1/repos/"+storetest.Repo+"/security", "", nil), 200)
	assert.Equal(t, agent.do("GET", "/api/v1/repos/secret/security", "", nil), 404)
	assert.Equal(t, agent.do("GET", "/api/v1/repos", "", nil, OrgHeader, side.Slug), 404, "a key cannot switch organisation")

	two := signup("dan@example.com")
	var danOrg accounts.Org
	require.Equal(t, two.do("POST", "/api/v1/accounts/orgs", `{"name":"Dan two"}`, &danOrg), 201)
	assert.Equal(t, two.do("POST", "/api/v1/repos", `{"url":"https://github.com/acme/x"}`, nil), 400, "several organisations: say which one")

	// Deleting a shared repository only removes the organisation's link.
	assert.Equal(t, bob.do("DELETE", "/api/v1/repos/"+storetest.Repo, "", nil), 204)
	assert.Equal(t, bob.do("GET", "/api/v1/repos/"+storetest.Repo+"/overview", "", nil), 404, "gone for the team")
	assert.Equal(t, ada.do("GET", "/api/v1/repos/"+storetest.Repo+"/overview", "", nil), 200, "the graph stays for the other organisation")
	links, err := acc.St.GraphRepositories(ctx, storetest.Repo)
	require.NoError(t, err)
	assert.Equal(t, len(links), 1)
}
