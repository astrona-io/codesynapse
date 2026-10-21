package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/store/memory"
	"github.com/parisnakitakejser/codesynapse/internal/store/storetest"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

// fakeGitHub serves one organisation "acme" with four repositories.
func fakeGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orgs/acme/repos" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, `[
			{"name":"api","default_branch":"main"},
			{"name":"web","default_branch":"main","private":true},
			{"name":"old","archived":true},
			{"name":"fork-of-x","fork":true}
		]`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type orgFixture struct {
	ts     *httptest.Server
	mu     sync.Mutex
	cloned []string
}

func newOrgServer(t *testing.T) *orgFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	f := &orgFixture{}
	srv := New(ctx, Config{Workspace: t.TempDir(), GitHubAPI: fakeGitHub(t).URL, Version: "test"}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.cloneHook = func(remote, name string) (Job, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.cloned = append(f.cloned, name+" "+remote)
		return Job{Repo: name, Kind: "clone", State: JobRunning}, nil
	}
	f.ts = httptest.NewServer(srv)
	t.Cleanup(f.ts.Close)
	return f
}

type orgJSON struct {
	Org    string         `json:"org"`
	Counts map[string]int `json:"counts"`
	Repos  []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"repos"`
}

func TestOrgAddScansListsAndQueuesClones(t *testing.T) {
	f := newOrgServer(t)
	assert.Equal(t, post(t, f.ts, "/api/v1/orgs", `{"org":"../x"}`), 400)
	assert.Equal(t, post(t, f.ts, "/api/v1/orgs", `{"org":"nobody"}`), 404)

	assert.Equal(t, post(t, f.ts, "/api/v1/orgs", `{"org":"https://github.com/acme/*"}`), 201, "accepts the astrona-io/* form")
	assert.Equal(t, post(t, f.ts, "/api/v1/orgs", `{"org":"acme"}`), 409)

	f.mu.Lock()
	assert.Equal(t, f.cloned, []string{
		"api https://github.com/acme/api.git",
		"web https://github.com/acme/web.git",
	}, "archived repositories and forks are skipped by default")
	f.mu.Unlock()

	var list struct {
		Orgs []orgJSON `json:"orgs"`
	}
	require.Equal(t, get(t, f.ts, "/api/v1/orgs", &list), 200)
	require.Len(t, list.Orgs, 1)
	assert.Equal(t, list.Orgs[0].Counts["excluded"], 2)
	assert.Equal(t, list.Orgs[0].Counts["failed"]+list.Orgs[0].Counts["pending"]+list.Orgs[0].Counts["indexing"], 2)
	assert.Len(t, list.Orgs[0].Repos, 4, "every repository is listed, including excluded ones")
}

func TestOrgWithoutAutoIndexOnlyListsUntilAsked(t *testing.T) {
	f := newOrgServer(t)
	assert.Equal(t, post(t, f.ts, "/api/v1/orgs", `{"org":"acme","auto_index":false,"include_archived":true}`), 201)
	f.mu.Lock()
	assert.Len(t, f.cloned, 0)
	f.mu.Unlock()

	var list struct {
		Orgs []orgJSON `json:"orgs"`
	}
	require.Equal(t, get(t, f.ts, "/api/v1/orgs", &list), 200)
	assert.Equal(t, list.Orgs[0].Counts["available"], 3, "api, web and the archived one")

	assert.Equal(t, post(t, f.ts, "/api/v1/orgs/acme/repos/api/index", ``), 202)
	assert.Equal(t, post(t, f.ts, "/api/v1/orgs/acme/repos/not-listed/index", ``), 404, "only listed repositories can be cloned")
	f.mu.Lock()
	assert.Equal(t, f.cloned, []string{"api https://github.com/acme/api.git"})
	f.mu.Unlock()

	assert.Equal(t, put(t, f.ts, "/api/v1/orgs/acme", `{"auto_index":true,"include_archived":false,"include_forks":false}`), 200)
	assert.Equal(t, post(t, f.ts, "/api/v1/orgs/acme/scan", ``), 200)
	f.mu.Lock()
	assert.Len(t, f.cloned, 3, "turning auto-index on queues the rest on the next scan")
	f.mu.Unlock()

	assert.Equal(t, del(t, f.ts, "/api/v1/orgs/acme"), 204)
	assert.Equal(t, del(t, f.ts, "/api/v1/orgs/acme"), 404)
}

func TestOrgsPersistAndComeDue(t *testing.T) {
	dir := t.TempDir()
	o, err := loadOrgs(dir)
	require.NoError(t, err)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(t, o.put(Org{Org: "Acme", NextScan: now.Add(5 * time.Minute).Format(time.RFC3339)}))
	require.NoError(t, o.put(Org{Org: "beta"}))
	assert.Equal(t, o.due(now), []string{"beta"}, "never scanned means due; the other is not due yet")
	assert.Equal(t, o.due(now.Add(5*time.Minute)), []string{"Acme", "beta"})

	again, err := loadOrgs(dir)
	require.NoError(t, err)
	got, ok := again.get("acme")
	assert.True(t, ok, "lookups ignore case")
	assert.Equal(t, got.Org, "Acme")
	assert.True(t, again.begin("acme"))
	assert.True(t, !again.begin("ACME"), "one scan at a time per organisation")
}
