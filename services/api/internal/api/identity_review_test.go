package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/curation"
	"github.com/astrona-io/codesynapse/internal/identity"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/query"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestHumanIdentityReviewAPI(t *testing.T) {
	ctx := context.Background()
	db := memory.New()
	build := func(name, head string) *model.Graph {
		g := model.NewGraph()
		g.AddNode(model.Node{ID: model.NodeID("r", model.KindRepository, "r"), Repo: "r", Name: "r", Kind: model.KindRepository, Head: head})
		g.AddNode(model.Node{ID: model.NodeID("r", model.KindFunction, name), Repo: "r", Name: name, Kind: model.KindFunction, Language: "go", File: name + ".go", BodyHash: name})
		return g
	}
	g := build("Old", "one")
	first, err := identity.Reconcile(g, "r", identity.Report{Repo: "r", Version: 1})
	require.NoError(t, err)
	g = build("Current", "two")
	report, err := identity.Reconcile(g, "r", first)
	require.NoError(t, err)
	rn, _ := g.Node(model.NodeID("r", model.KindRepository, "r"))
	rn.Identities, err = identity.Encode(report)
	require.NoError(t, err)
	require.NoError(t, db.ReplaceRepo(ctx, "r", g))
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	ts := httptest.NewServer(New(ctx, Config{Accounts: acc}, db, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)
	owner := newClient(t, ts)
	var signed struct {
		User accounts.User
		Org  accounts.Org
	}
	require.Equal(t, owner.do("POST", "/api/v1/auth/signup", `{"email":"identity-owner@example.com","name":"Owner","password":"`+pw+`"}`, &signed), 201)
	require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: signed.Org.ID, Graph: "r", AddedBy: signed.User.ID, AddedAt: time.Now()}))
	in := curation.IdentityReviewInput{From: report.Symbols[0].ID, To: report.Symbols[1].ID, Relation: "same-symbol", Reason: "Checked source history", Revision: "two"}
	body, err := json.Marshal(in)
	require.NoError(t, err)
	require.Equal(t, owner.do("POST", "/api/v1/repos/r/identities/reviews", string(body), nil), 201)
	var card query.SymbolCard
	require.Equal(t, owner.do("GET", "/api/v1/repos/r/symbol?id="+in.From, "", &card), 200)
	assert.Equal(t, card.Symbol.SymbolID, in.To)
	assert.Equal(t, card.ResolvedFrom, in.From)
	var key struct{ Key string }
	require.Equal(t, owner.do("POST", "/api/v1/me/keys", `{"org":"`+signed.Org.Slug+`","name":"identity-test"}`, &key), 201)
	agent := newClient(t, ts)
	agent.auth = "Bearer " + key.Key
	assert.Equal(t, agent.do("POST", "/api/v1/repos/r/identities/reviews", string(body), nil), 403, "even admin keys cannot perform human identity review")
	assert.Equal(t, agent.do("GET", "/api/v1/repos/r/identities/reviews", "", nil), 200)
	events, err := acc.St.ListEvents(ctx, accounts.EventFilter{})
	require.NoError(t, err)
	found := false
	for _, e := range events {
		found = found || e.Action == "identity.review" && e.UserID == signed.User.ID && e.OrgID == signed.Org.ID
	}
	assert.True(t, found)
}
