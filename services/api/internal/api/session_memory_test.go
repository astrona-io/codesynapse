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
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/sessionmemory"
	memstore "github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

type runtimeFixture struct {
	client   *client
	server   *Server
	accounts *accounts.Service
	store    *memstore.Store
	user     accounts.User
	org      accounts.Org
	identity execution.Identity
}

func newRuntimeFixture(t *testing.T, cfg Config) runtimeFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	st := memstore.New()
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	u, org, err := acc.SignUp(ctx, "runtime@example.com", "Runtime", pw)
	require.NoError(t, err)
	require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: org.ID, Graph: storetest.Repo, AddedBy: u.ID, AddedAt: time.Now()}))
	key, _, err := acc.CreateKey(ctx, accounts.Principal{User: u, Via: "session"}, org.ID, "runtime", 0)
	require.NoError(t, err)
	cfg.Accounts = acc
	srv := New(ctx, cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	c := newClient(t, ts)
	c.auth = "Bearer " + key
	var id execution.Identity
	require.Equal(t, c.do("POST", "/api/v1/execution/sessions", `{"session_id":"thread-a"}`, &id), 201)
	return runtimeFixture{c, srv, acc, st, u, org, id}
}

func runtimeHeaders(session string) []string {
	return []string{execution.SessionHeader, session, execution.ParentSessionHeader, session}
}

func TestSessionMemoryIsolationAndPromotion(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	path := "/api/v1/repos/" + storetest.Repo + "/session-memory"
	triple := `{"triples":[{"subject":"Payments","predicate":"OWNED_BY","object":"Team Blue"}]}`
	assert.Equal(t, f.client.do("POST", path, triple, nil), 403, "missing runtime context cannot write")
	assert.Equal(t, f.client.do("POST", path, triple, nil, execution.SessionHeader, "thread-a", execution.ParentSessionHeader, "forged"), 403)
	require.Equal(t, f.client.do("POST", path, triple, nil, runtimeHeaders("thread-a")...), 200)
	var a memory.Graph
	require.Equal(t, f.client.do("GET", path, "", &a, runtimeHeaders("thread-a")...), 200)
	require.Len(t, a.Facts, 1)
	require.Equal(t, f.client.do("POST", "/api/v1/execution/sessions", `{"session_id":"thread-b"}`, nil), 201)
	var b memory.Graph
	require.Equal(t, f.client.do("GET", path, "", &b, runtimeHeaders("thread-b")...), 200)
	assert.Len(t, b.Facts, 0)
	durable, err := f.store.SearchMemory(t.Context(), memory.Namespace(f.org.ID, storetest.Repo), "", 100, false)
	require.NoError(t, err)
	assert.Len(t, durable.Facts, 0, "session writes do not become repository knowledge")
	body, err := json.Marshal(map[string]any{"fact_ids": []string{a.Facts[0].ID}})
	require.NoError(t, err)
	assert.Equal(t, f.client.do("POST", path+"/promote", string(body), nil, runtimeHeaders("thread-b")...), 404, "opaque IDs cannot cross sessions")
	require.Equal(t, f.client.do("POST", path+"/promote", string(body), nil, runtimeHeaders("thread-a")...), 200)
	durable, err = f.store.SearchMemory(t.Context(), memory.Namespace(f.org.ID, storetest.Repo), "", 100, false)
	require.NoError(t, err)
	require.Len(t, durable.Facts, 1)
	assert.Equal(t, durable.Facts[0].OriginSessionID, "thread-a")
	require.Equal(t, f.client.do("DELETE", "/api/v1/execution/sessions/thread-a", "", nil), 204)
	assert.Equal(t, f.client.do("GET", path, "", nil, runtimeHeaders("thread-a")...), 403)
	assert.Equal(t, f.client.do("POST", "/api/v1/execution/sessions", `{"session_id":"thread-a"}`, nil), 409, "revoked primary IDs cannot be recycled")
	require.Equal(t, f.client.do("GET", path, "", &b, runtimeHeaders("thread-b")...), 200)
	ctx, err := execution.With(t.Context(), f.identity)
	require.NoError(t, err)
	_, err = f.store.ReadSessionMemory(ctx, sessionmemory.Scope{TenantID: f.org.ID, SessionID: "thread-a", Namespace: memory.Namespace(f.org.ID, storetest.Repo)}, "", 100)
	require.Error(t, err, "revocation removes session data and retains its tombstone")
	durable, err = f.store.SearchMemory(t.Context(), memory.Namespace(f.org.ID, storetest.Repo), "", 100, false)
	require.NoError(t, err)
	assert.Len(t, durable.Facts, 1, "explicit promotion survives session deletion")
}

func TestRuntimeBindingRejectsOtherParticipants(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	ctx := t.Context()
	other, _, err := f.accounts.SignUp(ctx, "other@example.com", "Other", pw)
	require.NoError(t, err)
	require.NoError(t, f.accounts.St.SetMember(ctx, f.org.ID, other.ID, accounts.Member))
	key, _, err := f.accounts.CreateKey(ctx, accounts.Principal{User: other, Via: "session"}, f.org.ID, "other", 0)
	require.NoError(t, err)
	otherClient := newClient(t, f.client.ts)
	otherClient.auth = "Bearer " + key
	path := "/api/v1/repos/" + storetest.Repo + "/session-memory"
	assert.Equal(t, otherClient.do("GET", path, "", nil, runtimeHeaders("thread-a")...), 404)
	assert.Equal(t, otherClient.do("POST", "/api/v1/execution/sessions", `{"session_id":"thread-a"}`, nil), 409)
	assert.Equal(t, otherClient.do("DELETE", "/api/v1/execution/sessions/thread-a", "", nil), 409)
	assert.Equal(t, f.client.do("POST", "/api/v1/execution/sessions", `{"session_id":"bad' scope"}`, nil), 400)
	require.NoError(t, f.accounts.St.RemoveMember(ctx, f.org.ID, f.user.ID))
	assert.Equal(t, f.client.do("GET", path, "", nil, runtimeHeaders("thread-a")...), 403, "API keys lose access with membership")
}
