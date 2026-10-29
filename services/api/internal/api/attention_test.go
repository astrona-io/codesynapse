package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/usage"
)

// riskyGraph has two critical and one high finding and a vulnerability the
// code calls.
func riskyGraph(name string) *model.Graph {
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID(name, model.KindRepository, name), Kind: model.KindRepository, Repo: name, Name: name, Time: time.Now().UTC().Format(time.RFC3339)})
	for i, sev := range []string{"critical", "critical", "high"} {
		g.AddNode(model.Node{ID: model.NodeID(name, model.KindFinding, string(rune('a'+i))), Kind: model.KindFinding, Repo: name, Name: "f", Severity: sev, Rule: "exec-variable", File: "main.go", StartLine: i + 1})
	}
	g.AddNode(model.Node{ID: model.NodeID(name, model.KindVulnerability, "GO-1"), Kind: model.KindVulnerability, Repo: name, Name: "GO-1", Severity: "high", Reachability: "called"})
	return g
}

func TestAttentionRanksWhatMatters(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, "risky", riskyGraph("risky")))
	quiet := model.NewGraph()
	// Ingest records what the security rules could check; nothing here.
	quiet.AddNode(model.Node{ID: model.NodeID("quiet", model.KindRepository, "quiet"), Kind: model.KindRepository, Repo: "quiet", Name: "quiet",
		Time: time.Now().UTC().Format(time.RFC3339), Coverage: "{}"})
	require.NoError(t, st.ReplaceRepo(ctx, "quiet", quiet))
	ts := httptest.NewServer(New(ctx, Config{}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)

	var a Attention
	require.Equal(t, newClient(t, ts).do("GET", "/api/v1/attention", "", &a), 200)
	require.True(t, len(a.Items) >= 2, "%+v", a.Items)
	for i := 1; i < len(a.Items); i++ {
		assert.True(t, levelRank[a.Items[i-1].Level] <= levelRank[a.Items[i].Level], "worst first")
	}
	byTitle := map[string]AttentionItem{}
	for _, it := range a.Items {
		assert.True(t, it.Link != "" && it.Action != "" && it.Detail != "", "every item says what to do: %+v", it)
		byTitle[it.Title] = it
	}
	crit, ok := byTitle["2 critical security findings"]
	require.True(t, ok, "%+v", a.Items)
	assert.Equal(t, crit.Level, "critical")
	assert.Equal(t, crit.Link, "/repos/risky/security")
	_, ok = byTitle["1 high-severity security finding"]
	assert.True(t, ok)
	called, ok := byTitle["1 vulnerable dependency your code calls"]
	require.True(t, ok, "%+v", a.Items)
	assert.Equal(t, called.Level, "critical")
	assert.Equal(t, a.Items[0].Kind, "vulnerability", "called vulnerabilities rank first")
	assert.Equal(t, a.Repos, 2)
	var quietItems []AttentionItem
	for _, it := range a.Items {
		if it.Repo == "quiet" {
			quietItems = append(quietItems, it)
		}
	}
	require.Len(t, quietItems, 1, "an empty repository has no grade: not checked is not clean")
	assert.Equal(t, quietItems[0].Title, "Not covered by the security checks")
	require.Equal(t, newClient(t, ts).do("GET", "/api/v1/attention?repo=quiet", "", &a), 200)
	assert.Equal(t, a.Repos, 1, "?repo= limits the work to one repository")
	assert.Len(t, a.Items, 1)
}

func TestAttentionNamesLostAccessToItsOrganisation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	ts := httptest.NewServer(New(ctx, Config{Accounts: acc}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)
	newClient(t, ts).signUp(`{"email":"ada@example.com","name":"Ada","password":"`+pw+`"}`, nil)
	bob := newClient(t, ts)
	var signed struct {
		Org  accounts.Org
		User accounts.User
	}
	require.Equal(t, bob.signUp(`{"email":"bob@example.com","name":"Bob","password":"`+pw+`"}`, &signed), 202)
	require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: signed.Org.ID, AddedBy: signed.User.ID, Graph: "acme/secret", Remote: "https://github.com/acme/secret", Visibility: accounts.Private, AddedAt: time.Now()}))
	require.NoError(t, acc.St.SetRepositoryAccess(ctx, signed.Org.ID, "acme/secret", accounts.AccessLost, time.Now()))

	var a Attention
	require.Equal(t, bob.do("GET", "/api/v1/attention", "", &a), 200)
	require.Len(t, a.Items, 1)
	assert.Equal(t, a.Items[0].Kind, "access")
	assert.Equal(t, a.Items[0].Link, "/settings/git-tokens")
	carol := newClient(t, ts)
	require.Equal(t, carol.signUp(`{"email":"carol@example.com","name":"Carol","password":"`+pw+`"}`, nil), 202)
	require.Equal(t, carol.do("GET", "/api/v1/attention", "", &a), 200)
	assert.Len(t, a.Items, 0, "other organisations do not learn about it")
}

func TestAttentionFlagsDuplicateCode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, "org/a", dupGraph(t, "org/a", "One", "Two", "Three", "Four")))
	require.NoError(t, st.ReplaceRepo(ctx, "org/b", dupGraph(t, "org/b", "Five", "Six", "Seven", "Eight")))
	ts := httptest.NewServer(New(ctx, Config{}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)

	var a Attention
	require.Equal(t, newClient(t, ts).do("GET", "/api/v1/attention", "", &a), 200)
	var perRepo, shared []AttentionItem
	for _, it := range a.Items {
		if it.Kind != "quality" {
			continue
		}
		if it.Repo == "" {
			shared = append(shared, it)
		} else {
			perRepo = append(perRepo, it)
		}
	}
	require.Len(t, perRepo, 2, "both repositories repeat most of their code")
	assert.Equal(t, perRepo[0].Link[len(perRepo[0].Link)-len("/duplicates"):], "/duplicates")
	require.Len(t, shared, 1)
	assert.Equal(t, shared[0].Link, "/shared-code")
	assert.True(t, shared[0].Detail != "")

	require.Equal(t, newClient(t, ts).do("GET", "/api/v1/attention?repo=org%2Fa", "", &a), 200)
	for _, it := range a.Items {
		assert.True(t, it.Kind != "quality" || it.Repo == "org/a", "?repo= limits to one repository and skips the cross-repository advice: %+v", it)
	}
}

func TestAttentionCountsWhatAgentsSaved(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, "risky", riskyGraph("risky")))
	today := usage.Day(time.Now())
	require.NoError(t, st.AddUsage(ctx, []usage.Bucket{
		{Day: today, Repo: "risky", Tool: "get_symbol_context", Client: "claude-code", Calls: 10, ResponseTokens: 500},
		{Day: today, Repo: "risky", Tool: "upsert_graph_triples", Client: "claude-code", Calls: 3, ResponseTokens: 90},
	}))
	ts := httptest.NewServer(New(ctx, Config{}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)

	var a Attention
	require.Equal(t, newClient(t, ts).do("GET", "/api/v1/attention", "", &a), 200)
	assert.Equal(t, a.AgentDays, 30)
	assert.Equal(t, a.AgentCalls, 10) // navigation calls only, not memory writes
	assert.Equal(t, a.TokensReturned, 500)
}

func TestAgentUsageUsesEachRepositorysRatio(t *testing.T) {
	repos := []RepoSummary{{Name: "r", GraphTokens: 100, BaseTokens: 1000}}
	now := time.Now()
	u := agentUsage([]usage.Bucket{
		{Day: usage.Day(now), Repo: "r", Tool: "get_symbol_context", Calls: 10, ResponseTokens: 500},
		{Day: usage.Day(now), Repo: "r", Tool: "get_symbol_context", Calls: 10, Errors: 10, ResponseTokens: 500},
	}, repos, 30, now)
	// Ratio 10: each returned token stands for 9 more that file reading
	// would have cost; the failed calls answered nothing.
	assert.Equal(t, u.AvoidedTokens, 500*9)
}

// A member's own private repository that lost access is that member's to
// see: another member of the organisation must not learn its name.
func TestLostAccessShowsPersonalEntriesToTheirOwnerOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	ada, org, err := acc.SignUp(ctx, "ada@example.com", "Ada", pw)
	require.NoError(t, err)
	srv := New(ctx, Config{Accounts: acc}, memory.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, r := range []accounts.Repository{
		{Graph: "acme/shared"},
		{Graph: "ada/diary", OwnerID: ada.ID},
		{Graph: "ada/working", OwnerID: ada.ID},
	} {
		r.OrgID, r.AddedBy, r.Visibility, r.Remote, r.AddedAt = org.ID, ada.ID, accounts.Private, "https://github.com/"+r.Graph, time.Now()
		require.NoError(t, acc.St.AddRepository(ctx, r))
		if r.Graph != "ada/working" {
			require.NoError(t, acc.St.SetRepositoryAccess(ctx, org.ID, r.Graph, accounts.AccessLost, time.Now()))
		}
	}
	links := map[string]string{"acme/shared": "/settings/git-tokens", "ada/diary": "/settings/profile?tab=git"}

	for _, tc := range []struct {
		name   string
		caller string
		want   []string
	}{
		{"the owner sees their own and the organisation's", ada.ID, []string{"acme/shared", "ada/diary"}},
		{"another member sees the organisation's only", "bob", []string{"acme/shared"}},
		{"no principal sees the organisation's only", "", []string{"acme/shared"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := context.WithValue(ctx, scopeKey{}, scope{orgs: []accounts.Membership{{Org: org, Role: accounts.Viewer}}})
			if tc.caller != "" {
				c = context.WithValue(c, principalKey{}, accounts.Principal{User: accounts.User{ID: tc.caller}, Via: "session"})
			}
			lost, err := srv.lostAccess(c)
			require.NoError(t, err)
			var got []string
			for _, l := range lost {
				got = append(got, l.Graph)
				item := lostAccessItem(l)
				assert.Equal(t, item.Link, links[l.Graph], l.Graph)
				assert.Equal(t, item.Kind, "access")
			}
			slices.Sort(got)
			assert.Equal(t, strings.Join(got, ","), strings.Join(tc.want, ","))
		})
	}
	org1 := lostAccessItem(accounts.Repository{Graph: "acme/shared"})
	assert.Equal(t, org1.Title, "Your organisation can no longer read this private repository", "organisation entries keep their wording")
}
