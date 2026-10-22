package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"

	"github.com/parisnakitakejser/codesynapse/internal/execution"
	"github.com/parisnakitakejser/codesynapse/internal/gitx"
	agentmem "github.com/parisnakitakejser/codesynapse/internal/memory"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/reviews"
	"github.com/parisnakitakejser/codesynapse/internal/store/memory"
	"github.com/parisnakitakejser/codesynapse/internal/store/storetest"
)

func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	sources := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(sources, "proj"), 0o750))
	srv := New(ctx, Config{SourcesRoot: sources, Version: "test"}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, sources
}

func get(t *testing.T, ts *httptest.Server, path string, out any) int {
	t.Helper()
	res, err := http.Get(ts.URL + path) //nolint:noctx // test
	require.NoError(t, err)
	defer res.Body.Close() //nolint:errcheck // test
	if out != nil && res.StatusCode == 200 {
		require.NoError(t, json.NewDecoder(res.Body).Decode(out))
	}
	return res.StatusCode
}

func post(t *testing.T, ts *httptest.Server, path, body string) int {
	t.Helper()
	res, err := http.Post(ts.URL+path, "application/json", strings.NewReader(body)) //nolint:noctx // test
	require.NoError(t, err)
	defer res.Body.Close() //nolint:errcheck // test
	return res.StatusCode
}

// Handler tests use an explicit trusted runtime fixture; HTTP tests separately
// verify that callers without a registered runtime are refused at ingress.
func runtimeHandlerStatus(t *testing.T, handler http.HandlerFunc, method, path, repo, body string) int {
	t.Helper()
	ctx, err := execution.With(context.Background(), fixedExecution())
	require.NoError(t, err)
	req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("repo", repo)
	w := httptest.NewRecorder()
	handler(w, req)
	return w.Code
}

func TestReadEndpoints(t *testing.T) {
	ts, _ := newTestServer(t)
	base := "/api/v1/repos/" + storetest.Repo

	var dash Dashboard
	require.Equal(t, get(t, ts, base+"/dashboard", &dash), 200)
	assert.Equal(t, dash.Stats.Tests, 1)
	require.NotEmpty(t, dash.MostCalled)
	assert.Equal(t, dash.MostCalled[0].Node.Name, "Handler")
	assert.Len(t, dash.Security, 1)

	var graph GraphView
	require.Equal(t, get(t, ts, base+"/graph?id=Handler", &graph), 200)
	assert.Greater(t, len(graph.Nodes), 3)
	assert.Greater(t, len(graph.Edges), 2)

	tests := []struct {
		path string
		want int
	}{
		{"/healthz", 200},
		{"/api/v1/repos", 200},
		{base + "/overview", 200},
		{base + "/search?q=do", 200},
		{base + "/search", 400},
		{base + "/search?q=x&kind=Nope", 400},
		{base + "/symbol?id=Do", 409}, // ambiguous: interface and concrete method
		{base + "/symbol?id=" + url.QueryEscape(storetest.ID("Function", "app.Handler")), 200},
		{base + "/outline?file=app/app.go", 200},
		{base + "/list?kind=Function&metric=complexity", 200},
		{base + "/list?metric=bogus", 400},
		{base + "/hotspots", 200},
		{base + "/packages/graph", 200},
		{"/api/v1/repos/nope/dashboard", 404},
		{base + "/licenses", 404}, // fixture repository has no stored report
		{base + "/duplicates", 404},
		{"/api/v1/shared-code", 200},
		{"/api/v1/sources", 200},
		{"/api/v1/settings", 200},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			assert.Equal(t, get(t, ts, tt.path, nil), tt.want)
		})
	}
}

func TestImpact(t *testing.T) {
	ts, _ := newTestServer(t)
	diff := "diff --git a/app/app.go b/app/app.go\n--- a/app/app.go\n+++ b/app/app.go\n@@ -10 +10 @@\n"
	body, err := json.Marshal(map[string]string{"diff": diff})
	require.NoError(t, err)
	assert.Equal(t, post(t, ts, "/api/v1/repos/"+storetest.Repo+"/impact", string(body)), 200)
	assert.Equal(t, post(t, ts, "/api/v1/repos/"+storetest.Repo+"/impact", `{"diff":"x","extra":1}`), 400)
}

func TestIngestRejectsEscapes(t *testing.T) {
	ts, sources := newTestServer(t)
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(sources, "link")))
	for _, body := range []string{
		`{"source":"../etc"}`,
		`{"source":"/etc"}`,
		`{"source":"link"}`,
		`{"source":"missing"}`,
		`{"source":"proj","repo":"bad name"}`,
		`{"source":""}`,
	} {
		t.Run(body, func(t *testing.T) {
			assert.Equal(t, post(t, ts, "/api/v1/ingest", body), 400)
		})
	}
}

func TestLicensesEndpoint(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID("r", model.KindRepository, "r"), Kind: model.KindRepository, Repo: "r", Name: "r",
		License: "MIT", Licenses: `{"project":{"license":"MIT","source":"LICENSE"},"total":3,"conflict":1}`})
	require.NoError(t, st.ReplaceRepo(ctx, "r", g))
	ts := httptest.NewServer(New(ctx, Config{}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)
	var got struct {
		Project  struct{ License string }
		Conflict int
	}
	require.Equal(t, get(t, ts, "/api/v1/repos/r/licenses", &got), 200)
	assert.Equal(t, got.Project.License, "MIT")
	assert.Equal(t, got.Conflict, 1)
}

func TestMemoryEndpoints(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	b, err := agentmem.Prepare(storetest.Repo, []agentmem.Triple{{Subject: "team/blue", Predicate: "OWNS", Object: "Payments"}}, "t", time.Now())
	require.NoError(t, err)
	require.NoError(t, st.UpsertMemory(ctx, b))
	srv := New(ctx, Config{}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	base := "/api/v1/repos/" + storetest.Repo + "/memory"
	var g agentmem.Graph
	require.Equal(t, get(t, ts, base+"?q=payments", &g), 200)
	require.Len(t, g.Facts, 1)
	assert.Equal(t, del(t, ts, base), 403, "runtime identity is required at HTTP ingress")
	remove := func(path string) int {
		return runtimeHandlerStatus(t, srv.handleDeleteMemory, http.MethodDelete, path, storetest.Repo, "")
	}
	assert.Equal(t, remove(base), 400)
	assert.Equal(t, remove(base+"?fact=nope"), 404)
	assert.Equal(t, remove(base+"?fact="+g.Facts[0].ID), 204)
	assert.Equal(t, remove(base+"?entity="+url.QueryEscape(agentmem.EntityID(storetest.Repo, "team/blue"))), 204, "entity IDs may contain slashes")
	require.Equal(t, get(t, ts, base, &g), 200)
	assert.Equal(t, len(g.Entities), 1)
}

func TestMaskToken(t *testing.T) {
	tests := []struct{ in, want string }{
		{"ghp_abcdefghijklmnopqrstuvwxyz0123456789", "ghp_a***56789"},
		{"glpat-0123456789abcdefghij", "glpat***fghij"},
		{"short-token-123", "***"},
		{"", "***"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			assert.Equal(t, maskToken(tt.in), tt.want)
		})
	}
}

func TestSettingsTokens(t *testing.T) {
	// Only CODESYNAPSE_GIT_TOKENS set, as `just git-token` writes it: the
	// legacy single-token field stays empty.
	srv := New(context.Background(), Config{
		GitAuth: gitx.Auth{Host: "github.com"},
		GitAuths: []gitx.Auth{
			{Host: "GitHub.com", Token: "ghp_abcdefghijklmnopqrstuvwxyz0123456789"},
			{Host: "github.com", Token: "ghp_second_token_never_used_0000000000"},
			{Host: "codeberg.org", Token: "0123456789abcdefghijklmn"},
		},
	}, memory.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	var got struct {
		Configured bool       `json:"git_token_configured"`
		Hosts      []string   `json:"git_token_hosts"`
		Tokens     []gitToken `json:"git_tokens"`
	}
	require.Equal(t, get(t, ts, "/api/v1/settings", &got), 200)
	assert.True(t, got.Configured)
	assert.Equal(t, got.Hosts, []string{"github.com", "codeberg.org"})
	assert.Equal(t, got.Tokens, []gitToken{{Host: "github.com", Preview: "ghp_a***56789"}, {Host: "codeberg.org", Preview: "01234***jklmn"}})
}

func TestValidRepoName(t *testing.T) {
	assert.True(t, validRepoName("codesynapse_v1.2"))
	assert.False(t, validRepoName(".hidden"))
	assert.False(t, validRepoName("."))
	assert.False(t, validRepoName(".."))
	assert.True(t, validRepoName("astrona-io/astrona-cli"), "owner/name")
	assert.True(t, validRepoName("codeberg.org/team/api"), "host/owner/name")
	assert.False(t, validRepoName("a/b/c/d"))
	assert.False(t, validRepoName("a/../b"))
	assert.False(t, validRepoName(strings.Repeat("a", 101)))
}

func TestGraphExplorerEndpoints(t *testing.T) {
	ts, _ := newTestServer(t)
	base := "/api/v1/repos/" + storetest.Repo

	var overview GraphView
	require.Equal(t, get(t, ts, base+"/graph/overview?limit=10", &overview), 200)
	assert.Greater(t, len(overview.Nodes), 2)
	assert.NotEmpty(t, overview.Edges)

	var expanded GraphView
	require.Equal(t, get(t, ts, base+"/graph/expand?id=Handler", &expanded), 200)
	names := map[string]bool{}
	for _, n := range expanded.Nodes {
		names[n.Name] = true
	}
	assert.True(t, names["app.main"], "caller")
	assert.True(t, names["Usage"], "mentioning doc section")
	assert.True(t, names["app.go"], "defining file")

	assert.Equal(t, get(t, ts, base+"/graph/overview?edge=BOGUS", nil), 400)

	ids, err := json.Marshal(map[string]any{"ids": []string{storetest.ID("Function", "app.main"), storetest.ID("Function", "app.Handler")}})
	require.NoError(t, err)
	assert.Equal(t, post(t, ts, base+"/graph/edges", string(ids)), 200)
	assert.Equal(t, post(t, ts, base+"/graph/edges", `{"ids":["other|Function|x"]}`), 400)
}

func TestScanContent(t *testing.T) {
	dir := t.TempDir()
	for p, body := range map[string]string{
		"main.go": "package main", "svc/app.py": "", "web/a.ts": "", "web/a.d.ts": "", "web/node_modules/x/i.js": "",
		"chart/Chart.yaml": "", "chart/templates/d.yaml": "", "README.md": "", ".github/workflows/ci.yml": "", "docker-compose.yml": "",
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, p), []byte(body), 0o600))
	}
	c, truncated := scanContent(dir)
	assert.False(t, truncated)
	assert.Equal(t, c["go"], 1)
	assert.Equal(t, c["python"], 1)
	assert.Equal(t, c["typescript"], 1, ".d.ts and node_modules skipped")
	assert.Equal(t, c["javascript"], 0)
	assert.Equal(t, c["helm"], 1)
	assert.Equal(t, c["markdown"], 1)
	assert.Equal(t, c["github-actions"], 1)
	assert.Equal(t, c["compose"], 1)
	assert.Equal(t, c["yaml"], 4)
}

func TestSyncAndRegister(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sources := t.TempDir()
	proj := filepath.Join(sources, "proj")
	require.NoError(t, os.Mkdir(proj, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(proj, "README.md"), []byte("# Proj\n"), 0o600))

	st := memory.New()
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID("proj", model.KindRepository, "proj"), Kind: model.KindRepository, Repo: "proj", Name: "proj", Root: proj, Branch: "main"})
	require.NoError(t, st.ReplaceRepo(ctx, "proj", g))
	srv := New(ctx, Config{SourcesRoot: sources, Workspace: t.TempDir()}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv)
	defer ts.Close()

	assert.Equal(t, post(t, ts, "/api/v1/repos/proj/sync", `{}`), 403, "missing runtime is refused at ingress")
	assert.Equal(t, runtimeHandlerStatus(t, srv.handleSync, http.MethodPost, "/api/v1/repos/proj/sync", "proj", `{}`), http.StatusForbidden, "accountsless jobs cannot validate the persisted runtime")
	assert.Equal(t, runtimeHandlerStatus(t, srv.handleSync, http.MethodPost, "/api/v1/repos/proj/sync", "proj", `{"branch":"feature"}`), 403, "runtime authorization precedes branch changes")
	assert.Equal(t, post(t, ts, "/api/v1/repos/nope/sync", `{}`), 403)

	for _, body := range []string{
		`{"url":"http://github.com/a/b"}`,
		`{"url":"file:///etc/passwd"}`,
		`{"url":"https://user:pw@github.com/a/b"}`,
		`{"url":"https://127.0.0.1/a/b"}`,
		`{"url":"https://github.com/a/b","branch":"--upload-pack=x"}`,
	} {
		assert.Equal(t, post(t, ts, "/api/v1/repos", body), 403, "runtime is checked before repository validation")
		assert.Equal(t, runtimeHandlerStatus(t, srv.handleRegister, http.MethodPost, "/api/v1/repos", "", body), 400, body)
	}
}

func TestSwitchToRemoteRefusals(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sources, ws := t.TempDir(), t.TempDir()
	st := memory.New()
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID("proj", model.KindRepository, "proj"), Kind: model.KindRepository, Repo: "proj", Name: "proj", Root: filepath.Join(sources, "proj"), Branch: "main"})
	require.NoError(t, st.ReplaceRepo(ctx, "proj", g))
	g2 := model.NewGraph()
	g2.AddNode(model.Node{ID: model.NodeID("cloned", model.KindRepository, "cloned"), Kind: model.KindRepository, Repo: "cloned", Name: "cloned", Root: filepath.Join(ws, "cloned"), URL: "https://github.com/a/cloned"})
	require.NoError(t, st.ReplaceRepo(ctx, "cloned", g2))
	srv := New(ctx, Config{SourcesRoot: sources, Workspace: ws}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv)
	defer ts.Close()

	assert.Equal(t, post(t, ts, "/api/v1/repos/proj/remote", `{}`), 403, "missing runtime is refused at ingress")
	remote := func(repo, body string) int {
		return runtimeHandlerStatus(t, srv.handleSwitchToRemote, http.MethodPost, "/api/v1/repos/"+repo+"/remote", repo, body)
	}
	assert.Equal(t, remote("proj", `{}`), 400, "no origin and no URL")
	assert.Equal(t, remote("proj", `{"url":"http://github.com/a/b"}`), 400, "https only")
	assert.Equal(t, remote("proj", `{"url":"https://127.0.0.1/a/b"}`), 400, "no internal hosts")
	assert.Equal(t, remote("cloned", `{}`), 400, "already a clone")
	require.NoError(t, os.Mkdir(filepath.Join(ws, "proj"), 0o750))
	assert.Equal(t, remote("proj", `{"url":"https://github.com/a/proj"}`), http.StatusConflict, "folder exists")
}

func TestWorkspaceOverview(t *testing.T) {
	ts, _ := newTestServer(t)
	var ov Overview
	require.Equal(t, get(t, ts, "/api/v1/overview", &ov), 200)
	assert.Equal(t, ov.Totals.Repos, 1)
	require.Len(t, ov.Repos, 1)
	assert.Equal(t, ov.Repos[0].Name, storetest.Repo)
	assert.Greater(t, ov.Totals.Nodes, 5)
	assert.Equal(t, ov.Totals.NodeKinds["Function"], 3)
	assert.Equal(t, ov.Repos[0].Grade, "A")
}

func TestJobHealth(t *testing.T) {
	h := jobHealth([]Job{
		{State: JobSucceeded, StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:00:10Z"},
		{State: JobFailed, StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:00:30Z", Error: "boom"},
		{State: JobRunning, StartedAt: "2026-01-01T00:01:00Z"},
	})
	assert.Equal(t, h.Succeeded, 1)
	assert.Equal(t, h.Failed, 1)
	assert.Equal(t, h.Running, 1)
	assert.Equal(t, h.AvgDurationSec, 20.0)
	assert.Equal(t, h.MaxDurationSec, 30.0)
	assert.Len(t, h.RecentFailures, 1)
}

func put(t *testing.T, ts *httptest.Server, path, body string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, ts.URL+path, strings.NewReader(body)) //nolint:noctx // test
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close() //nolint:errcheck // test
	return res.StatusCode
}

func del(t *testing.T, ts *httptest.Server, path string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, ts.URL+path, nil) //nolint:noctx // test
	require.NoError(t, err)
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close() //nolint:errcheck // test
	return res.StatusCode
}

func TestScheduleAndDelete(t *testing.T) {
	ts, _ := newTestServer(t)
	base := "/api/v1/repos/" + storetest.Repo
	var sc struct {
		State           string `json:"state"`
		IntervalMinutes int    `json:"interval_minutes"`
		NextRun         string `json:"next_run"`
	}
	require.Equal(t, get(t, ts, base+"/schedule", &sc), 200)
	assert.Equal(t, sc.State, "active", "syncs every 30 minutes unless configured")
	assert.Equal(t, sc.IntervalMinutes, 30)
	assert.Equal(t, put(t, ts, base+"/schedule", `{"interval_minutes":0}`), 200)
	require.Equal(t, get(t, ts, base+"/schedule", &sc), 200)
	assert.Equal(t, sc.State, "manual", "turning it off sticks")

	assert.Equal(t, put(t, ts, base+"/schedule", `{"interval_minutes":60}`), 403, "enabling automation needs runtime delegation")
	srv := ts.Config.Handler.(*Server)
	assert.Equal(t, runtimeHandlerStatus(t, srv.handlePutSchedule, http.MethodPut, base+"/schedule", storetest.Repo, `{"interval_minutes":60}`), 403)
	require.Equal(t, get(t, ts, base+"/schedule", &sc), 200)
	assert.Equal(t, sc.State, "manual")

	assert.Equal(t, put(t, ts, base+"/schedule", `{"interval_minutes":60,"paused":true}`), 200)
	require.Equal(t, get(t, ts, base+"/schedule", &sc), 200)
	assert.Equal(t, sc.State, "paused")
	assert.Equal(t, put(t, ts, base+"/schedule", `{"interval_minutes":7}`), 400, "only allowed intervals")
	assert.Equal(t, put(t, ts, "/api/v1/repos/nope/schedule", `{"interval_minutes":60}`), 404)

	assert.Equal(t, del(t, ts, base), 204)
	assert.Equal(t, get(t, ts, base+"/dashboard", nil), 404)
	require.Equal(t, get(t, ts, base+"/schedule", &sc), 200)
	assert.Equal(t, sc.State, "active", "schedule removed with the repo; back to the default")
	assert.Equal(t, sc.IntervalMinutes, 30)
}

func TestSchedulesPersistAndDue(t *testing.T) {
	dir := t.TempDir()
	s, err := loadSchedules(dir)
	require.NoError(t, err)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	_, err = s.set("a", 15, false, false, false, now)
	require.NoError(t, err)
	_, err = s.set("b", 60, true, false, false, now)
	require.NoError(t, err)
	assert.Len(t, s.due(now.Add(10*time.Minute)), 0)
	assert.Equal(t, s.due(now.Add(16*time.Minute)), []string{"a"}, "paused schedules never run")
	s.started("a", now.Add(16*time.Minute), "started", nil)
	assert.Len(t, s.due(now.Add(20*time.Minute)), 0)

	again, err := loadSchedules(dir)
	require.NoError(t, err)
	assert.Equal(t, again.get("b").State(), "paused", "persisted to disk")
}

func TestSchedulesDefaultEveryThirtyMinutes(t *testing.T) {
	s, err := loadSchedules("")
	require.NoError(t, err)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	s.ensure("a", now)
	_, err = s.set("b", 0, false, false, false, now)
	require.NoError(t, err)
	s.ensure("b", now)
	assert.Equal(t, s.due(now.Add(30*time.Minute)), []string{"a"}, "default repos sync within 30 minutes; manual ones never")
}

func TestScheduleStaggerAndFinish(t *testing.T) {
	s, err := loadSchedules("")
	require.NoError(t, err)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	distinct := map[time.Duration]bool{}
	for i := range 40 {
		repo := fmt.Sprintf("org/repo-%d", i)
		d := stagger(repo, 30)
		assert.True(t, d >= 0 && d < 30*time.Minute, "offset stays inside one interval")
		distinct[d] = true
		s.ensure(repo, now)
	}
	assert.True(t, len(distinct) > 20, "first runs are spread out")
	assert.True(t, len(s.due(now.Add(5*time.Minute))) < 40, "not everything is due at once")

	s.started("org/repo-0", now, "started", nil)
	end := now.Add(12 * time.Minute) // a slow sync
	s.finished("org/repo-0", end, nil)
	assert.Equal(t, s.due(end.Add(29*time.Minute)) == nil || !slices.Contains(s.due(end.Add(29*time.Minute)), "org/repo-0"), true, "interval counts from the end of the sync")
	assert.True(t, slices.Contains(s.due(end.Add(30*time.Minute)), "org/repo-0"))
}

func TestReviewsAPI(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	rv, err := reviews.Prepare(reviews.Review{Repo: storetest.Repo, Title: "T", Comments: []reviews.CommentItem{{File: "app/app.go", Line: 9, Message: "m"}}}, time.Now())
	require.NoError(t, err)
	require.NoError(t, st.SaveReview(ctx, rv))
	srv := New(ctx, Config{}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv)
	defer ts.Close()

	var list struct{ Reviews []reviews.Summary }
	require.Equal(t, get(t, ts, "/api/v1/repos/"+storetest.Repo+"/reviews", &list), 200)
	require.Len(t, list.Reviews, 1)
	assert.Equal(t, list.Reviews[0].Counts.Open, 1)

	patch := func(path, body string) int {
		req, err := http.NewRequest(http.MethodPatch, ts.URL+path, strings.NewReader(body)) //nolint:noctx // test
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer res.Body.Close() //nolint:errcheck // test
		return res.StatusCode
	}
	cpath := "/api/v1/reviews/" + rv.ID + "/comments/" + rv.Comments[0].ID
	assert.Equal(t, patch(cpath, `{"status":"resolved","note":"done"}`), 200)
	assert.Equal(t, patch(cpath, `{"status":"maybe"}`), 400)
	assert.Equal(t, patch("/api/v1/reviews/nope/comments/x", `{"status":"resolved"}`), 404)
	assert.Equal(t, del(t, ts, "/api/v1/reviews/"+rv.ID), 204)
	assert.Equal(t, get(t, ts, "/api/v1/reviews/"+rv.ID, nil), 404)
}

func TestRecoverFixes(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	rv, err := reviews.Prepare(reviews.Review{Repo: storetest.Repo, Title: "r", Comments: []reviews.CommentItem{{File: "a.go", Message: "m"}, {File: "b.go", Message: "m"}}}, time.Now())
	require.NoError(t, err)
	reviews.SetFix(&rv, []string{rv.Comments[0].ID}, reviews.Fix{State: reviews.FixRunning}, time.Now())
	require.NoError(t, st.SaveReview(ctx, rv))
	s := &Server{st: st, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.recoverFixes(ctx)
	got, err := st.GetReview(ctx, rv.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Comments[0].Fix)
	assert.Equal(t, got.Comments[0].Fix.State, reviews.FixFailed)
	assert.Contains(t, got.Comments[0].Fix.Error, "restarted")
	assert.True(t, got.Comments[1].Fix == nil)
}
