package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	agentmem "github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/reviews"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestReBACOverHTTP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	for _, name := range []string{"acme/api", "acme/web"} {
		g := model.NewGraph()
		g.AddNode(model.Node{ID: model.NodeID(name, model.KindRepository, name), Kind: model.KindRepository, Repo: name, Name: name})
		require.NoError(t, st.ReplaceRepo(ctx, name, g))
	}
	acc := accounts.NewService(acctmem.New(), true)
	srv := New(ctx, Config{Accounts: acc}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	signup := func(email string) *client {
		c := newClient(t, ts)
		require.Equal(t, c.do("POST", "/api/v1/auth/signup", `{"email":"`+email+`","name":"`+email+`","password":"`+pw+`"}`, nil), 201)
		return c
	}
	ada := signup("ada@example.com") // platform admin
	bob := signup("bob@example.com")
	carol := signup("carol@example.com")
	dave := signup("dave@example.com")
	var acme, ext accounts.Org
	require.Equal(t, bob.do("POST", "/api/v1/accounts/orgs", `{"name":"Acme"}`, &acme), 201)
	require.Equal(t, dave.do("POST", "/api/v1/accounts/orgs", `{"name":"Ext"}`, &ext), 201)
	bobUser, err := acc.St.UserByEmail(ctx, "bob@example.com")
	require.NoError(t, err)
	for _, g := range []string{"acme/api", "acme/web"} {
		require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: acme.ID, Graph: g, AddedBy: bobUser.ID, AddedAt: time.Now()}))
	}
	require.Equal(t, bob.do("PUT", "/api/v1/accounts/orgs/"+acme.Slug+"/members", `{"email":"carol@example.com","role":"guest"}`, nil), 200)
	inAcme, inExt := []string{OrgHeader, acme.Slug}, []string{OrgHeader, ext.Slug}
	visible := func(c *client, org []string) []string {
		var list struct{ Repos []struct{ Name string } }
		require.Equal(t, c.do("GET", "/api/v1/repos", "", &list, org...), 200)
		out := []string{}
		for _, r := range list.Repos {
			out = append(out, r.Name)
		}
		return out
	}
	access := func(repo string) string {
		return "/api/v1/accounts/orgs/" + acme.Slug + "/repos/" + url.PathEscape(repo) + "/access"
	}
	repoPath := func(repo, rest string) string { return "/api/v1/repos/" + url.PathEscape(repo) + rest }

	// A guest starts with nothing; a grant opens one repository.
	assert.Equal(t, visible(carol, inAcme), []string{})
	require.Equal(t, bob.do("PUT", access("acme/web"), `{"user":"carol@example.com","role":"viewer"}`, nil), 200)
	assert.Equal(t, visible(carol, inAcme), []string{"acme/web"})
	assert.Equal(t, carol.do("GET", repoPath("acme/api", "/overview"), "", nil, inAcme...), 404, "ungranted repositories do not exist for her")
	assert.Equal(t, carol.do("POST", repoPath("acme/web", "/sync"), "", nil, inAcme...), 403, "viewers do not sync")

	// A team grant: editors of acme/api.
	require.Equal(t, bob.do("POST", "/api/v1/accounts/orgs/"+acme.Slug+"/teams", `{"name":"Contractors"}`, nil), 201)
	require.Equal(t, bob.do("PUT", "/api/v1/accounts/orgs/"+acme.Slug+"/teams/contractors/members", `{"email":"carol@example.com"}`, nil), 200)
	require.Equal(t, bob.do("PUT", access("acme/api"), `{"team":"`+acme.Slug+`/contractors","role":"member"}`, nil), 200)
	assert.Equal(t, visible(carol, inAcme), []string{"acme/api", "acme/web"})
	var mine struct {
		Repos []struct {
			Graph, Via string
			Role       accounts.Role
		}
	}
	require.Equal(t, carol.do("GET", "/api/v1/me/access", "", &mine), 200)
	for _, r := range mine.Repos {
		if r.Graph == "acme/api" {
			assert.Equal(t, r.Via, "team")
			assert.Equal(t, r.Role, accounts.Member)
		}
	}
	assert.Equal(t, carol.do("PUT", access("acme/web"), `{"user":"dave@example.com","role":"viewer"}`, nil), 403, "editors do not grant")
	assert.Equal(t, carol.do("POST", repoPath("acme/api", "/sync"), "", nil, inAcme...), 403, "writers cannot sync")
	require.Equal(t, bob.do("PUT", access("acme/api"), `{"team":"`+acme.Slug+`/contractors","role":"admin"}`, nil), 200)
	require.Equal(t, bob.do("PUT", access("acme/api"), `{"user":"carol@example.com","role":"viewer"}`, nil), 200)
	assert.Equal(t, carol.do("POST", repoPath("acme/api", "/sync"), "", nil, inAcme...), 403, "direct read overrides team admin")
	require.Equal(t, bob.do("PUT", access("acme/api"), `{"user":"carol@example.com","role":"none"}`, nil), 200)
	assert.Equal(t, carol.do("GET", repoPath("acme/api", "/overview"), "", nil, inAcme...), 404, "direct deny hides the repository")
	carolUser, err := acc.St.UserByEmail(ctx, "carol@example.com")
	require.NoError(t, err)
	require.Equal(t, bob.do("DELETE", access("acme/api")+"?subject=user:"+carolUser.ID, "", nil), 204)
	assert.Equal(t, visible(carol, inAcme), []string{"acme/api", "acme/web"}, "removal restores team inheritance")

	// External grants require explicit membership in the owning organisation.
	assert.Equal(t, bob.do("PUT", access("acme/web"), `{"user":"dave@example.com","role":"member"}`, nil), 403, "shares are read-only")
	assert.Equal(t, bob.do("PUT", access("acme/web"), `{"user":"dave@example.com","role":"viewer"}`, nil), 403)
	require.Equal(t, bob.do("PUT", "/api/v1/accounts/orgs/"+acme.Slug+"/members", `{"email":"dave@example.com","role":"guest"}`, nil), 200)
	require.Equal(t, bob.do("PUT", access("acme/web"), `{"user":"dave@example.com","role":"viewer"}`, nil), 200)
	assert.Equal(t, visible(dave, inExt), []string{}, "another organisation cannot use the grant")
	assert.Equal(t, visible(dave, inAcme), []string{"acme/web"})
	for _, rv := range []reviews.Review{
		{Repo: "acme/web", OrgID: acme.ID, Title: "Acme web review"},
		{Repo: "acme/api", OrgID: acme.ID, Title: "Acme api review"},
		{Repo: "acme/web", OrgID: "someone-else", Title: "Another organisation's review"},
	} {
		rv.Comments = []reviews.CommentItem{{File: "a.go", Line: 1, Message: "m"}}
		p, err := reviews.Prepare(rv, time.Now())
		require.NoError(t, err)
		require.NoError(t, st.SaveReview(ctx, p))
	}
	var list struct{ Reviews []struct{ Title string } }
	require.Equal(t, dave.do("GET", repoPath("acme/web", "/reviews"), "", &list, inAcme...), 200)
	titles := []string{}
	for _, r := range list.Reviews {
		titles = append(titles, r.Title)
	}
	assert.Equal(t, titles, []string{"Acme web review"}, "the share carries Acme's reviews of that repository, nothing else")
	b, err := agentmem.Prepare(agentmem.Namespace(acme.ID, "acme/web"), []agentmem.Triple{{Subject: "Checkout", Predicate: "OWNED_BY", Object: "Team Web"}}, "agent", time.Now())
	require.NoError(t, err)
	require.NoError(t, st.UpsertMemory(ctx, b))
	var mem string
	require.Equal(t, dave.do("GET", repoPath("acme/web", "/memory"), "", &mem, inAcme...), 200)
	assert.Contains(t, mem, "Checkout", "and its agent memory")
	assert.Equal(t, dave.do("POST", repoPath("acme/web", "/memory/offload"), `{"turns":[{"role":"user","text":"x"}]}`, nil, inAcme...), 403, "but cannot write to it")
	var shared string
	require.Equal(t, dave.do("GET", "/api/v1/me/access", "", &shared), 200)
	assert.Contains(t, shared, `"shared":false`)
	var grants string
	assert.Equal(t, dave.do("GET", access("acme/web"), "", &grants), 403, "recipients do not see who else has access")

	// Revoking the share closes it.
	var gs struct{ Grants []accounts.RepoGrant }
	require.Equal(t, bob.do("GET", access("acme/web"), "", &gs), 200)
	var daveID string
	for _, g := range gs.Grants {
		if g.Label == "dave@example.com" {
			daveID = g.SubjectID
			assert.True(t, !g.External)
		}
	}
	require.Equal(t, bob.do("DELETE", access("acme/web")+"?subject=user:"+daveID, "", nil), 204)
	assert.Equal(t, visible(dave, inExt), []string{})

	// Unlinking the repository removes its grants.
	require.Equal(t, bob.do("DELETE", repoPath("acme/web", "")+"?files=false", "", nil, inAcme...), 204)
	assert.Equal(t, visible(carol, inAcme), []string{"acme/api"})

	var audit string
	require.Equal(t, ada.do("GET", "/api/v1/admin/audit?limit=500&format=jsonl", "", &audit), 200)
	for _, a := range []string{"repo.grant.set", "repo.grant.remove", "team.create", "team.member.set"} {
		assert.True(t, strings.Contains(audit, a), "audited: "+a)
	}
}
