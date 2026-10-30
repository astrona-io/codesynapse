package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
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
	require.Equal(t, bob.signUp(`{"email":"bob@example.com","name":"Bob","password":"`+pw+`"}`, &signed), 202)
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

	events := func(action string) int {
		rows, err := acc.St.ListEvents(ctx, accounts.EventFilter{OrgID: org.ID, Action: action, Limit: 10})
		require.NoError(t, err)
		return len(rows)
	}
	unseen := func() {
		require.NoError(t, acc.St.RecordWatchScan(ctx, org.ID, created.Key, accounts.WatchScan{LastStatus: "ok", Known: []string{"old"}}))
	}
	member := func(v string) {
		_, _ = acc.St.RemoveRepository(ctx, org.ID, graph)
		require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: org.ID, Graph: graph, Visibility: v, OwnerID: signed.User.ID, AddedBy: signed.User.ID, AddedAt: time.Now()}))
	}

	// A member holds it privately: the organisation's watch has not read
	// it with an organisation token, so it skips it without an audit entry.
	member(accounts.Private)
	unseen()
	srv.scanWatch(ctx, watches(), time.Now().UTC(), logger)
	assert.Equal(t, watches().Added, 0, "a skip adds nothing")
	assert.Equal(t, watches().LastStatus, "ok")
	assert.Equal(t, events("watch.repo_add"), 0, "and leaves no watch.repo_add entry")
	rows, err := acc.St.GraphRepositories(ctx, graph)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.True(t, rows[0].Personal(), "it stays the member's")

	// A member's public entry is taken over for the organisation.
	member(accounts.Public)
	unseen()
	srv.scanWatch(ctx, watches(), time.Now().UTC(), logger)
	assert.Equal(t, linked(), 1, "the new public repository is added")
	assert.Equal(t, watches().Added, 1)
	assert.Equal(t, events("watch.repo_add"), 1)
	assert.Equal(t, events("repo.space_moved"), 1)
	rows, err = acc.St.GraphRepositories(ctx, graph)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.False(t, rows[0].Personal(), "the organisation's now")
	srv.scanWatch(ctx, watches(), time.Now().UTC(), logger)
	assert.Equal(t, watches().Added, 1, "it is not added twice")

	// Who may create and change watches.
	var key struct{ Key string }
	require.Equal(t, bob.do("POST", "/api/v1/me/keys", `{"org":"`+org.Slug+`","name":"laptop"}`, &key), 201)
	agent := newClient(t, ts)
	agent.auth = "Bearer " + key.Key
	assert.Equal(t, agent.do("POST", "/api/v1/watches", body, nil), 403, "a person sets a watch, an API key does not")
	eve := newClient(t, ts)
	require.Equal(t, eve.signUp(`{"email":"eve@example.com","name":"Eve","password":"`+pw+`"}`, nil), 202)
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

// refusingAudit refuses audit entries of one action while it is set.
type refusingAudit struct {
	accounts.Store
	mu     sync.Mutex
	action string
}

func (f *refusingAudit) refuse(action string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.action = action
}

func (f *refusingAudit) AddEvent(ctx context.Context, e accounts.Event) (accounts.Event, error) {
	f.mu.Lock()
	refused := f.action != "" && e.Action == f.action
	f.mu.Unlock()
	if refused {
		return accounts.Event{}, errors.New("disk full")
	}
	return f.Store.AddEvent(ctx, e)
}

func TestOrgWatchAddsPrivateRepositoriesWithTheOrganisationsToken(t *testing.T) {
	const token = "ghp_watch_token_123456"
	prev := canRead
	canRead = func(_ context.Context, remote string, auth gitx.Auth) error {
		if !strings.Contains(remote, "private") || auth.Token == token {
			return nil
		}
		return gitx.ErrNoAccess
	}
	t.Cleanup(func() { canRead = prev })

	var (
		mu       sync.Mutex
		private  = []string{"old-private"}
		lastAuth = "unset"
	)
	listPrivate := func(names ...string) {
		mu.Lock()
		defer mu.Unlock()
		private = append(private, names...)
	}
	auth := func() string {
		mu.Lock()
		defer mu.Unlock()
		return lastAuth
	}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orgs/acme/repos" || r.URL.Query().Get("page") > "1" {
			_, _ = fmt.Fprint(w, `[]`)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		lastAuth = r.Header.Get("Authorization")
		out := []string{`{"name":"open","default_branch":"main"}`}
		if lastAuth != "" {
			for _, p := range private {
				out = append(out, `{"name":"`+p+`","default_branch":"main","private":true}`)
			}
		}
		_, _ = fmt.Fprint(w, "["+strings.Join(out, ",")+"]")
	}))
	t.Cleanup(gh.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	// Indexed already for the same remote, so adding one links and does not clone.
	for _, name := range []string{"new-private", "audit-private", "carol-private", "late-private"} {
		graph := "127.0.0.1/acme/" + name
		g := model.NewGraph()
		g.AddNode(model.Node{ID: model.NodeID(graph, model.KindRepository, graph), Kind: model.KindRepository, Repo: graph, Name: graph, URL: "https://127.0.0.1/acme/" + name + ".git"})
		require.NoError(t, st.ReplaceRepo(ctx, graph, g))
	}
	audit := &refusingAudit{Store: acctmem.New()}
	acc := accounts.NewService(audit, true)
	bootstrapAccounts(t, acc)
	key := make([]byte, 32)
	key[5] = 9
	sealer, err := accounts.NewSealer(key)
	require.NoError(t, err)
	acc.Secrets = sealer
	srv := New(ctx, Config{Accounts: acc, Workspace: t.TempDir(), AllowPrivateGit: true, ForgeAPI: gh.URL}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	bob := newClient(t, ts)
	require.Equal(t, bob.signUp(`{"email":"bob@example.com","name":"Bob","password":"`+pw+`"}`, nil), 202)
	var org accounts.Org
	require.Equal(t, bob.do("POST", "/api/v1/accounts/orgs", `{"name":"Platform Team"}`, &org), 201)
	in := []string{OrgHeader, org.Slug}
	// Created before the organisation has a token: an anonymous baseline.
	require.Equal(t, bob.do("POST", "/api/v1/watches", `{"provider":"github","host":"127.0.0.1","org":"acme"}`, nil, in...), 201)
	assert.Equal(t, auth(), "", "no token yet")

	watch := func() accounts.OrgWatch {
		all, err := acc.St.OrgWatches(ctx, org.ID)
		require.NoError(t, err)
		require.Len(t, all, 1)
		return all[0]
	}
	assert.True(t, watch().PrivateBaseline.IsZero())
	events := func(action string) []accounts.Event {
		rows, err := acc.St.ListEvents(ctx, accounts.EventFilter{OrgID: org.ID, Action: action, Limit: 50})
		require.NoError(t, err)
		return rows
	}
	linked := func() map[string]accounts.Repository {
		repos, err := acc.St.OrgRepositories(ctx, org.ID)
		require.NoError(t, err)
		out := map[string]accounts.Repository{}
		for _, r := range repos {
			out[r.Graph] = r
		}
		return out
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scan := func(t *testing.T) {
		t.Helper()
		srv.scanWatch(ctx, watch(), time.Now().UTC(), logger)
		require.Equal(t, watch().LastStatus, "ok", watch().LastError)
	}
	setToken := func() {
		require.Equal(t, bob.do("PUT", "/api/v1/accounts/orgs/"+org.Slug+"/git-tokens/127.0.0.1", `{"token":"`+token+`"}`, nil), 200)
	}
	newest := func(events []accounts.Event) accounts.Event {
		return slices.MaxFunc(events, func(a, b accounts.Event) int { return cmp.Compare(a.ID, b.ID) })
	}

	setToken()
	t.Run("the first scan with the token takes older private repositories as baseline", func(t *testing.T) {
		scan(t)
		assert.Equal(t, auth(), "Bearer "+token, "an owner's watch lists with the organisation's token")
		assert.Len(t, events("git_token.list"), 1)
		assert.Len(t, linked(), 0, "a private repository from before the watch could see it is not added")
		assert.False(t, watch().PrivateBaseline.IsZero())
		assert.True(t, slices.Contains(watch().Known, "old-private"))
	})

	t.Run("a new private repository is added for the organisation", func(t *testing.T) {
		listPrivate("new-private")
		scan(t)
		got := linked()
		require.Len(t, got, 1)
		e := got["127.0.0.1/acme/new-private"]
		assert.Equal(t, e.Visibility, accounts.Private)
		assert.False(t, e.Personal(), "into the organisation's space")
		assert.Equal(t, watch().Added, 1)
		lists, adds := events("git_token.list"), events("watch.repo_add")
		require.Len(t, lists, 2)
		require.Len(t, adds, 1)
		assert.True(t, newest(lists).ID < adds[0].ID, "the token listing is audited before the add")
		assert.Equal(t, newest(lists).Via, "automation")
	})

	t.Run("without the git_token.list entry it lists anonymously", func(t *testing.T) {
		audit.refuse("git_token.list")
		listPrivate("audit-private")
		scan(t)
		audit.refuse("")
		assert.Equal(t, auth(), "")
		assert.Len(t, events("git_token.list"), 2)
		assert.Len(t, linked(), 1)
	})

	t.Run("without a token it lists anonymously", func(t *testing.T) {
		require.Equal(t, bob.do("DELETE", "/api/v1/accounts/orgs/"+org.Slug+"/git-tokens/127.0.0.1", "", nil), 204)
		scan(t)
		assert.Equal(t, auth(), "")
		assert.Len(t, linked(), 1)
	})

	t.Run("a replaced token takes a new baseline", func(t *testing.T) {
		before := watch().PrivateBaseline
		setToken()
		scan(t)
		assert.Equal(t, auth(), "Bearer "+token)
		assert.Len(t, linked(), 1, "what the new token shows on its first scan is baseline")
		assert.True(t, watch().PrivateBaseline.After(before))
		assert.True(t, slices.Contains(watch().Known, "audit-private"))
	})

	var carolUser struct{ User accounts.User }
	carol := newClient(t, ts)
	require.Equal(t, carol.signUp(`{"email":"carol@example.com","name":"Carol","password":"`+pw+`"}`, &carolUser), 202)
	invite(t, bob, org.Slug, carol, "carol@example.com", "admin")
	require.Equal(t, carol.do("PUT", "/api/v1/watches/127.0.0.1/acme", `{"paused":false}`, nil, in...), 200)
	require.Equal(t, watch().ActorID, carolUser.User.ID)

	t.Run("an admin creator lists with the token", func(t *testing.T) {
		listPrivate("carol-private")
		scan(t)
		assert.Equal(t, auth(), "Bearer "+token)
		assert.Len(t, linked(), 2)
		assert.Equal(t, newest(events("git_token.list")).UserID, carolUser.User.ID, "audited as the creator")
	})

	t.Run("a creator who is no longer an admin lists anonymously", func(t *testing.T) {
		require.Equal(t, bob.do("PUT", "/api/v1/accounts/orgs/"+org.Slug+"/members", `{"user_id":"`+carolUser.User.ID+`","role":"member"}`, nil), 200)
		n := len(events("git_token.list"))
		listPrivate("late-private")
		scan(t)
		assert.Equal(t, auth(), "", "a member's watch never uses the organisation's token")
		assert.Len(t, events("git_token.list"), n)
		assert.Len(t, linked(), 2)
		assert.False(t, slices.Contains(watch().Known, "late-private"), "left for a scan that can see it")
	})
}

func TestOrgWatchCreatedWithTheTokenHasAPrivateBaseline(t *testing.T) {
	const token = "ghp_watch_token_123456"
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") > "1" || r.Header.Get("Authorization") != "Bearer "+token {
			_, _ = fmt.Fprint(w, `[]`)
			return
		}
		_, _ = fmt.Fprint(w, `[{"name":"open"},{"name":"secret","private":true}]`)
	}))
	t.Cleanup(gh.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	key := make([]byte, 32)
	key[5] = 9
	sealer, err := accounts.NewSealer(key)
	require.NoError(t, err)
	acc.Secrets = sealer
	srv := New(ctx, Config{Accounts: acc, Workspace: t.TempDir(), AllowPrivateGit: true, ForgeAPI: gh.URL}, memory.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	bob := newClient(t, ts)
	require.Equal(t, bob.signUp(`{"email":"bob@example.com","name":"Bob","password":"`+pw+`"}`, nil), 202)
	var org accounts.Org
	require.Equal(t, bob.do("POST", "/api/v1/accounts/orgs", `{"name":"Platform Team"}`, &org), 201)
	require.Equal(t, bob.do("PUT", "/api/v1/accounts/orgs/"+org.Slug+"/git-tokens/127.0.0.1", `{"token":"`+token+`"}`, nil), 200)
	require.Equal(t, bob.do("POST", "/api/v1/watches", `{"provider":"github","host":"127.0.0.1","org":"acme"}`, nil, OrgHeader, org.Slug), 201)
	all, err := acc.St.OrgWatches(ctx, org.ID)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, all[0].Known, []string{"open", "secret"}, "an owner's watch starts from what the token reads")
	assert.False(t, all[0].PrivateBaseline.IsZero())
	events, err := acc.St.ListEvents(ctx, accounts.EventFilter{OrgID: org.ID, Action: "git_token.list"})
	require.NoError(t, err)
	assert.Len(t, events, 1, "the listing with the token is audited")
}
