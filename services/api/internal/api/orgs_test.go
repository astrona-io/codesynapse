package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/execution"

	"github.com/parisnakitakejser/codesynapse/internal/forge"
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
		if r.URL.Query().Get("page") > "1" { // Forgejo pages until an empty one
			_, _ = fmt.Fprint(w, `[]`)
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
	srv := New(ctx, Config{Workspace: t.TempDir(), ForgeAPI: fakeGitHub(t).URL, Version: "test"}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.cloneHook = func(remote, name string) (Job, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.cloned = append(f.cloned, name+" "+remote)
		return Job{Repo: name, Kind: "clone", State: JobRunning}, nil
	}
	f.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, err := execution.With(r.Context(), fixedExecution())
		require.NoError(t, err)
		srv.ServeHTTP(w, r.WithContext(ctx))
	}))
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
	assert.Equal(t, o.due(now.Add(5*time.Minute)), []string{"acme", "beta"}, "keyed by lower-case login")

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
		"": false, "a/b/c": true, "a/b/c/d": false, "/x": false, "x/": false, "../x": false, "a/..": false, ".hidden/x": false, "a b": false, "a\\b": false,
	} {
		assert.Equal(t, validRepoName(in), want, in)
	}
	for in, want := range map[string]string{
		"https://github.com/Astrona-IO/astrona-cli.git": "astrona-io/astrona-cli",
		"https://github.com/astrona-io/astrona-cli":     "astrona-io/astrona-cli",
		"https://gitlab.com/group/sub/proj.git":         "",
		"https://codeberg.org/Team/API.git":             "codeberg.org/team/API",
		"https://git.example.com/team/api":              "git.example.com/team/api",
		"https://git.example.com:8443/team/api":         "",
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
	assert.Equal(t, sc.NextRun, now.Add(35*time.Minute).Format(time.RFC3339), "an unchanged check still waits a full interval")
}

func TestSameOrgOnTwoProvidersStaysApart(t *testing.T) {
	f := newOrgServer(t)
	assert.Equal(t, post(t, f.ts, "/api/v1/orgs", `{"org":"acme"}`), 201)
	assert.Equal(t, post(t, f.ts, "/api/v1/orgs", `{"org":"git.example.com/acme","provider":"forgejo"}`), 201, "same login, another host")
	assert.Equal(t, post(t, f.ts, "/api/v1/orgs", `{"org":"https://git.example.com/acme/*","provider":"forgejo"}`), 409)
	assert.Equal(t, post(t, f.ts, "/api/v1/orgs", `{"org":"acme","host":"git.example.com"}`), 400, "an unknown host needs a provider")
	assert.Equal(t, post(t, f.ts, "/api/v1/orgs", `{"org":"acme","host":"host:3000","provider":"forgejo"}`), 400)

	f.mu.Lock()
	assert.Equal(t, f.cloned, []string{
		"acme/api https://github.com/acme/api.git",
		"acme/web https://github.com/acme/web.git",
		"git.example.com/acme/api https://git.example.com/acme/api.git",
		"git.example.com/acme/web https://git.example.com/acme/web.git",
	}, "github.com keeps owner/name; other hosts are prefixed with the host")
	f.mu.Unlock()

	var list struct {
		Orgs []struct {
			Key      string `json:"key"`
			Provider string `json:"provider"`
			Host     string `json:"host"`
		} `json:"orgs"`
	}
	require.Equal(t, get(t, f.ts, "/api/v1/orgs", &list), 200)
	require.Len(t, list.Orgs, 2)
	assert.Equal(t, list.Orgs[0].Key, "acme")
	assert.Equal(t, list.Orgs[0].Provider, "github")
	assert.Equal(t, list.Orgs[1].Key, "git.example.com/acme")
	assert.Equal(t, list.Orgs[1].Provider, "forgejo")

	assert.Equal(t, del(t, f.ts, "/api/v1/orgs/git.example.com%2Facme"), 204)
	assert.Equal(t, get(t, f.ts, "/api/v1/orgs", &list), 200)
	assert.Len(t, list.Orgs, 1, "removing one leaves the other host's organisation")
}

func TestParseOrgInput(t *testing.T) {
	for _, tc := range []struct {
		provider, host, in string
		kind               forge.Kind
		wantHost, wantOrg  string
		fails              bool
	}{
		{in: "astrona-io", kind: forge.GitHub, wantHost: "github.com", wantOrg: "astrona-io"},
		{in: "https://github.com/astrona-io/*", kind: forge.GitHub, wantHost: "github.com", wantOrg: "astrona-io"},
		{in: "astrona-io/*", kind: forge.GitHub, wantHost: "github.com", wantOrg: "astrona-io"},
		{in: "https://codeberg.org/team", kind: forge.Forgejo, wantHost: "codeberg.org", wantOrg: "team"},
		{in: "https://GitLab.com/grp/", kind: forge.GitLab, wantHost: "gitlab.com", wantOrg: "grp"},
		{provider: "gitea", in: "git.example.com/team", kind: forge.Forgejo, wantHost: "git.example.com", wantOrg: "team"},
		{provider: "github", host: "ghe.corp", in: "eng", kind: forge.GitHub, wantHost: "ghe.corp", wantOrg: "eng"},
		{in: "team", host: "git.example.com", fails: true},
		{in: "http://github.com/x", fails: true},
		{in: "https://user:pw@github.com/x", fails: true},
		{in: "https://git.example.com:8443/x", provider: "forgejo", fails: true},
		{in: "../x", fails: true},
		{in: "codeberg.org/team/repo", provider: "forgejo", fails: true},
		{in: "x", provider: "bitbucket", fails: true},
	} {
		kind, host, org, err := parseOrgInput(tc.provider, tc.host, tc.in)
		if tc.fails {
			assert.Error(t, err, tc.in)
			continue
		}
		require.NoError(t, err, tc.in)
		assert.Equal(t, kind, tc.kind, tc.in)
		assert.Equal(t, host, tc.wantHost, tc.in)
		assert.Equal(t, org, tc.wantOrg, tc.in)
	}
}

func TestLegacyOrgsDefaultToGitHub(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".codesynapse"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".codesynapse", "orgs.json"), []byte(`[{"org":"Old","auto_index":true,"repos":[]}]`), 0o600))
	o, err := loadOrgs(dir)
	require.NoError(t, err)
	got, ok := o.get("old")
	require.True(t, ok)
	assert.Equal(t, got.Provider, forge.GitHub)
	assert.Equal(t, got.Host, "github.com")
	assert.Equal(t, got.fullName("x"), "old/x")
}

func TestMissingRepositoryIsRemovedAfterThreeConfirmedChecks(t *testing.T) {
	ctx, err := execution.With(context.Background(), fixedExecution())
	require.NoError(t, err)
	exists := false
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/api" && exists:
			_, _ = fmt.Fprint(w, `{}`)
		case r.URL.Path == "/repos/acme/flaky":
			w.WriteHeader(http.StatusBadGateway)
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()

	ws := t.TempDir()
	st := memory.New()
	add := func(name, remote string) string {
		root := filepath.Join(ws, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(root, 0o700))
		g := model.NewGraph()
		g.AddNode(model.Node{ID: model.NodeID(name, model.KindRepository, name), Kind: model.KindRepository, Repo: name, Name: name, Root: root, URL: remote})
		require.NoError(t, st.ReplaceRepo(ctx, name, g))
		return root
	}
	root := add("acme/api", "https://github.com/acme/api.git")
	add("acme/flaky", "https://github.com/acme/flaky.git")
	mounted := t.TempDir() // outside the workspace: never removed
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID("mnt", model.KindRepository, "mnt"), Kind: model.KindRepository, Repo: "mnt", Name: "mnt", Root: mounted, URL: "https://github.com/acme/mnt.git"})
	require.NoError(t, st.ReplaceRepo(ctx, "mnt", g))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(ctx, Config{Workspace: ws, ForgeAPI: fake.URL, Version: "test"}, st, logger)
	for _, r := range []string{"acme/api", "acme/flaky", "mnt"} {
		srv.sched.ensure(r, time.Now())
	}
	has := func(name string) bool {
		_, err := st.Node(ctx, model.NodeID(name, model.KindRepository, name))
		return err == nil
	}

	srv.reapIfGone(ctx, "acme/api", logger)
	srv.reapIfGone(ctx, "acme/api", logger)
	assert.True(t, has("acme/api"), "two checks are not enough")
	exists = true
	srv.reapIfGone(ctx, "acme/api", logger)
	assert.Equal(t, srv.sched.get("acme/api").Missing, 0, "it exists again: the count starts over")
	exists = false
	for range goneStrikes {
		srv.reapIfGone(ctx, "acme/api", logger)
	}
	assert.True(t, !has("acme/api"), "three confirmed 404s in a row remove the graph")
	_, err = os.Stat(root)
	assert.True(t, os.IsNotExist(err), "and the clone")
	assert.Equal(t, srv.sched.get("acme/api").Missing, 0)

	for range 2 * goneStrikes {
		srv.reapIfGone(ctx, "acme/flaky", logger)
		srv.reapIfGone(ctx, "mnt", logger)
	}
	assert.True(t, has("acme/flaky"), "an unclear answer (502) never counts as gone")
	assert.True(t, has("mnt"), "mounted sources are never removed")
	_, err = os.Stat(mounted)
	assert.NoError(t, err)
}
