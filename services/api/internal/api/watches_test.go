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
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestOrgWatchAddsNewPublicRepositories(t *testing.T) {
	prev := canRead
	canRead = func(context.Context, string, gitx.Auth) error { return nil }
	t.Cleanup(func() { canRead = prev })

	const graph = "127.0.0.1/acme/api"
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	// Indexed already for the same remote, so adding it links and does not clone.
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID(graph, model.KindRepository, graph), Kind: model.KindRepository, Repo: graph, Name: graph, URL: "https://127.0.0.1/acme/api.git"})
	require.NoError(t, st.ReplaceRepo(ctx, graph, g))
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	srv := New(ctx, Config{Accounts: acc, Workspace: t.TempDir(), AllowPrivateGit: true, ForgeAPI: fakeGitHub(t).URL}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	bob := newClient(t, ts)
	var signed struct{ User accounts.User }
	require.Equal(t, bob.do("POST", "/api/v1/auth/signup", `{"email":"bob@example.com","name":"Bob","password":"`+pw+`"}`, &signed), 201)
	var org accounts.Org
	require.Equal(t, bob.do("POST", "/api/v1/accounts/orgs", `{"name":"Platform Team"}`, &org), 201)
	in := []string{OrgHeader, org.Slug}
	list := func(c *client, headers ...string) (int, struct{ Watches []WatchView }) {
		var out struct{ Watches []WatchView }
		return c.do("GET", "/api/v1/watches", "", &out, headers...), out
	}

	const body = `{"provider":"github","host":"127.0.0.1","org":"acme"}`
	var created WatchView
	require.Equal(t, bob.do("POST", "/api/v1/watches", body, &created, in...), 201)
	assert.Equal(t, created.Key, "127.0.0.1/acme")
	code, got := list(bob, in...)
	require.Equal(t, code, 200)
	require.Len(t, got.Watches, 1)
	assert.True(t, got.Watches[0].Mine)
	assert.Equal(t, got.Watches[0].Added, 0)

	watches := func() accounts.OrgWatch {
		all, err := acc.St.OrgWatches(ctx, org.ID)
		require.NoError(t, err)
		require.Len(t, all, 1)
		return all[0]
	}
	linked := func() int {
		repos, err := acc.St.OrgRepositories(ctx, org.ID)
		require.NoError(t, err)
		return len(repos)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv.scanWatch(ctx, watches(), time.Now().UTC(), logger)
	assert.Equal(t, linked(), 0, "what existed when the watch was created is not added")
	assert.Equal(t, watches().LastStatus, "ok")

	// A repository the watch has not seen: it is the one created since.
	require.NoError(t, acc.St.RecordWatchScan(ctx, org.ID, created.Key, accounts.WatchScan{LastStatus: "ok", Known: []string{"old"}}))
	srv.scanWatch(ctx, watches(), time.Now().UTC(), logger)
	assert.Equal(t, linked(), 1, "the new public repository is added")
	assert.Equal(t, watches().Added, 1)
	srv.scanWatch(ctx, watches(), time.Now().UTC(), logger)
	assert.Equal(t, watches().Added, 1, "it is not added twice")

	// Who may create and change watches.
	var key struct{ Key string }
	require.Equal(t, bob.do("POST", "/api/v1/me/keys", `{"org":"`+org.Slug+`","name":"laptop"}`, &key), 201)
	agent := newClient(t, ts)
	agent.auth = "Bearer " + key.Key
	assert.Equal(t, agent.do("POST", "/api/v1/watches", body, nil), 403, "a person sets a watch, an API key does not")
	eve := newClient(t, ts)
	require.Equal(t, eve.do("POST", "/api/v1/auth/signup", `{"email":"eve@example.com","name":"Eve","password":"`+pw+`"}`, nil), 201)
	code, _ = list(eve, in...)
	assert.Equal(t, code, 404, "other organisations cannot see it")
	assert.Equal(t, eve.do("DELETE", "/api/v1/watches/127.0.0.1/acme", "", nil, in...), 404)

	// A scan that cannot rebuild its creator's identity does nothing and says why.
	broken := watches()
	broken.Execution.SessionID, broken.Execution.ParentSessionID = "gone", "gone"
	srv.scanWatch(ctx, broken, time.Now().UTC(), logger)
	assert.Equal(t, watches().LastStatus, "needs sign-in")
	assert.Equal(t, watches().Added, 1)

	assert.Equal(t, bob.do("PUT", "/api/v1/watches/127.0.0.1/acme", `{"paused":true}`, nil, in...), 200)
	assert.True(t, watches().Paused)
	assert.Equal(t, bob.do("DELETE", "/api/v1/watches/127.0.0.1/acme", "", nil, in...), 204)
	_, got = list(bob, in...)
	assert.Equal(t, len(got.Watches), 0)
}
