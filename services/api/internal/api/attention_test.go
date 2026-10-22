package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/accounts"
	acctmem "github.com/parisnakitakejser/codesynapse/internal/accounts/memstore"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/store/memory"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
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
	ts := httptest.NewServer(New(ctx, Config{Accounts: acc}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)
	newClient(t, ts).do("POST", "/api/v1/auth/signup", `{"email":"ada@example.com","name":"Ada","password":"`+pw+`"}`, nil)
	bob := newClient(t, ts)
	var signed struct {
		Org  accounts.Org
		User accounts.User
	}
	require.Equal(t, bob.do("POST", "/api/v1/auth/signup", `{"email":"bob@example.com","name":"Bob","password":"`+pw+`"}`, &signed), 201)
	require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: signed.Org.ID, AddedBy: signed.User.ID, Graph: "acme/secret", Remote: "https://github.com/acme/secret", Visibility: accounts.Private, AddedAt: time.Now()}))
	require.NoError(t, acc.St.SetRepositoryAccess(ctx, signed.Org.ID, "acme/secret", accounts.AccessLost, time.Now()))

	var a Attention
	require.Equal(t, bob.do("GET", "/api/v1/attention", "", &a), 200)
	require.Len(t, a.Items, 1)
	assert.Equal(t, a.Items[0].Kind, "access")
	assert.Equal(t, a.Items[0].Link, "/settings/git-tokens")
	carol := newClient(t, ts)
	require.Equal(t, carol.do("POST", "/api/v1/auth/signup", `{"email":"carol@example.com","name":"Carol","password":"`+pw+`"}`, nil), 201)
	require.Equal(t, carol.do("GET", "/api/v1/attention", "", &a), 200)
	assert.Len(t, a.Items, 0, "other organisations do not learn about it")
}
