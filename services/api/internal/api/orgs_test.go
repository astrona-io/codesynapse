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

	"github.com/parisnakitakejser/codesynapse/internal/model"
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
		"acme/api https://github.com/acme/api.git",
		"acme/web https://github.com/acme/web.git",
	}, "named owner/name; archived repositories and forks are skipped by default")
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
	assert.Equal(t, f.cloned, []string{"acme/api https://github.com/acme/api.git"})
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

func TestOrgReposAreNamedWithTheirOwner(t *testing.T) {
	o := Org{Org: "Astrona-IO"}
	assert.Equal(t, o.fullName("cli"), "astrona-io/cli", "owner is lower-cased like GitHub logins")

	// A repository indexed before names carried the owner is matched by its
	// remote, so it is neither cloned again nor reported as a conflict.
	byName := map[string]model.Node{
		"cli":  {Name: "cli", URL: "https://github.com/astrona-io/cli.git"},
		"core": {Name: "core", URL: "https://github.com/someone-else/core.git"},
	}
	n, ok := o.indexedAs(OrgRepo{Name: "cli"}, byName)
	assert.True(t, ok)
	assert.Equal(t, n.Name, "cli")
	_, ok = o.indexedAs(OrgRepo{Name: "core"}, byName)
	assert.True(t, !ok, "same name, different owner: a different repository")

	byName["astrona-io/core"] = model.Node{Name: "astrona-io/core", URL: "https://github.com/astrona-io/core.git"}
	n, ok = o.indexedAs(OrgRepo{Name: "core"}, byName)
	assert.True(t, ok)
	assert.Equal(t, n.Name, "astrona-io/core", "the full name wins")
}

func TestRepoNames(t *testing.T) {
	for in, want := range map[string]bool{
		"api": true, "astrona-io/astrona-cli": true, "a.b/c_d": true,
		"": false, "a/b/c": false, "/x": false, "x/": false, "../x": false, "a/..": false, ".hidden/x": false, "a b": false, "a\\b": false,
	} {
		assert.Equal(t, validRepoName(in), want, in)
	}
	for in, want := range map[string]string{
		"https://github.com/Astrona-IO/astrona-cli.git": "astrona-io/astrona-cli",
		"https://github.com/astrona-io/astrona-cli":     "astrona-io/astrona-cli",
		"https://gitlab.com/group/sub/proj.git":         "sub/proj",
		"https://example.com/solo.git":                  "solo",
		"https://example.com/":                          "",
	} {
		assert.Equal(t, repoNameFromRemote(in), want, in)
	}
	assert.Equal(t, repoDir("/ws", "astrona-io/cli"), "/ws/astrona-io/cli")
}

func TestPruneKeepsRunningJobs(t *testing.T) {
	j := &Jobs{jobs: map[string]*Job{}}
	for i := range maxJobHistory + 10 {
		id := fmt.Sprint(i)
		j.jobs[id] = &Job{ID: id, State: JobRunning} // a burst of queued clones, none finished
	}
	j.prune() // used to panic: slice bounds out of range
	assert.Len(t, j.jobs, maxJobHistory+10)

	j.jobs["0"].State, j.jobs["0"].StartedAt = JobSucceeded, "2026-01-01T00:00:00Z"
	j.prune()
	assert.Len(t, j.jobs, maxJobHistory+9, "only finished jobs are dropped")
}

func TestScheduleRecordsUnchangedChecks(t *testing.T) {
	s, err := loadSchedules("")
	require.NoError(t, err)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s.ensure("a", now)
	s.started("a", now.Add(5*time.Minute), "unchanged", nil)
	sc := s.get("a")
	assert.Equal(t, sc.LastStatus, "unchanged")
	assert.Equal(t, sc.NextRun, now.Add(10*time.Minute).Format(time.RFC3339), "an unchanged check still waits a full interval")
}
