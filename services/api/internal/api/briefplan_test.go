package api

import (
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/asklocal"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// briefGraph is a small repository shaped like the one the sessions of PLAN
// item 18, cases h to k, ran in: the parts their prompts were about, and the
// names elsewhere that only share a word with them and were briefed instead.
func briefGraph(t *testing.T) *memory.Store {
	t.Helper()
	g := model.NewGraph()
	node := func(kind model.NodeKind, name, qualified, file string, start, end int) string {
		id := storetest.ID(kind, qualified)
		g.AddNode(model.Node{ID: id, Repo: storetest.Repo, Kind: kind, Name: name, QualifiedName: qualified,
			File: file, StartLine: start, EndLine: end, Exported: name[0] >= 'A' && name[0] <= 'Z'})
		return id
	}
	contains := func(from, to string) { g.AddEdge(model.Edge{From: from, To: to, Kind: model.EdgeContains}) }
	file := func(pkg, p string) string {
		id := node(model.KindFile, path.Base(p), p, p, 0, 0)
		contains(pkg, id)
		return id
	}
	// A module per front-end file, as the Vue/TS parser indexes them.
	page := func(p string, symbols ...string) {
		qualified := strings.TrimSuffix(p, path.Ext(p))
		pkg := node(model.KindPackage, path.Base(qualified), qualified, p, 0, 0)
		file(pkg, p)
		for i, s := range symbols {
			node(model.KindFunction, s, qualified+"."+s, p, 10+20*i, 20+20*i)
		}
	}
	goPkg := func(dir string) string {
		return node(model.KindPackage, path.Base(dir), "example.com/app/"+dir, "", 0, 0)
	}
	route := func(name, handler string) {
		id := node(model.KindRoute, name, name, "", 0, 0)
		g.AddEdge(model.Edge{From: id, To: handler, Kind: model.EdgeHandledBy})
	}

	// The documentation site (case h).
	node(model.KindConfig, "mkdocs.yml", "mkdocs.yml", "mkdocs.yml", 1, 150)
	hooks := node(model.KindPackage, "doc_status", "docs.hooks.doc_status", "docs/hooks/doc_status.py", 0, 0)
	file(hooks, "docs/hooks/doc_status.py")
	node(model.KindFunction, "on_page_markdown", "docs.hooks.doc_status.on_page_markdown", "docs/hooks/doc_status.py", 58, 65)
	node(model.KindSection, "Status of a page", "docs/index.md#status", "docs/index.md", 30, 41)
	metrics := goPkg("internal/metrics")
	file(metrics, "internal/metrics/metrics.go")
	node(model.KindType, "Status", "metrics.Status", "internal/metrics/metrics.go", 20, 40)
	node(model.KindFunction, "Verified", "metrics.Verified", "internal/metrics/metrics.go", 50, 70)
	node(model.KindFunction, "Draft", "metrics.Draft", "internal/metrics/metrics.go", 80, 90)

	// The gateway (case i): a package, its admin API and route.
	gw := goPkg("services/api/internal/gateway")
	file(gw, "services/api/internal/gateway/local.go")
	file(gw, "services/api/internal/gateway/gateway.go")
	node(model.KindFunction, "qualifies", "gateway.qualifies", "services/api/internal/gateway/local.go", 49, 69)
	node(model.KindType, "Gateway", "gateway.Gateway", "services/api/internal/gateway/gateway.go", 53, 91)
	api := goPkg("services/api/internal/api")
	file(api, "services/api/internal/api/gateway.go")
	file(api, "services/api/internal/api/series.go")
	gwHandler := node(model.KindMethod, "handleGatewaySettings", "api.Server.handleGatewaySettings", "services/api/internal/api/gateway.go", 251, 311)
	route("PUT /api/v1/accounts/orgs/{org}/gateway", gwHandler)
	route("POST /api/v1/usage/local", node(model.KindMethod, "handleLocalUsage", "api.Server.handleLocalUsage", "services/api/internal/api/usage_local.go", 116, 155))
	series := node(model.KindMethod, "handleSeries", "api.Server.handleSeries", "services/api/internal/api/series.go", 120, 180)
	route("GET /api/v1/stats/series", series)

	// The statistics pages (cases j and k).
	page("services/web/app/pages/stats.vue", "tabs", "days")
	page("services/web/app/pages/stats/activity.vue", "loadActivity")
	page("services/web/app/pages/stats/models.vue", "loadModelStats")

	// The model queue (case k).
	llm := goPkg("services/api/internal/llm")
	file(llm, "services/api/internal/llm/llm.go")
	file(llm, "services/api/internal/llm/queue.go")
	node(model.KindType, "limiter", "llm.limiter", "services/api/internal/llm/queue.go", 12, 40)
	node(model.KindMethod, "Waiting", "llm.Registry.Waiting", "services/api/internal/llm/llm.go", 590, 610)
	node(model.KindType, "ProviderConfig", "llm.ProviderConfig", "services/api/internal/llm/llm.go", 130, 160)

	// What the sessions were briefed with instead: front-end and chart files
	// and methods whose names are plain words of the prompts.
	page("services/web/app/pages/agents.vue", "agents", "waiting")
	page("services/web/app/pages/settings/organisation.vue", "page", "models")
	page("services/web/app/components/LineChart.vue", "line", "graph")
	page("services/web/app/components/ColumnChart.vue", "bar")
	page("services/web/app/components/DonutChart.vue", "local", "default")
	router := goPkg("internal/router")
	file(router, "internal/router/router.go")
	node(model.KindMethod, "local", "router.Table.local", "internal/router/router.go", 10, 20)
	node(model.KindMethod, "route", "router.Table.route", "internal/router/router.go", 30, 40)
	node(model.KindMethod, "first", "router.Table.first", "internal/router/router.go", 50, 60)

	st := memory.New()
	require.NoError(t, st.ReplaceRepo(t.Context(), storetest.Repo, g))
	return st
}

// TestBriefPlanFollowsWhatThePromptIsAbout: PLAN item 18, cases h to k
// (2026-10-29), each with the prompt of the session that was briefed wrong.
//
//   - h: a prompt about the documentation site naming mkdocs.yml and its
//     Python hook got internal/metrics/metrics.go: "page" made it a UI
//     prompt, and "draft", "status" and "verified" matched Go names. Files a
//     prompt names by path now lead, and a documentation prompt keeps to the
//     pages, site configuration and hooks.
//   - i: a gateway prompt with no code names got chart components and methods
//     called "local" and "route": every plain word was an exact term. The
//     words that name a package ("gateway") now lead, with its API handler.
//   - j: the gateway statistics tab got chart components; the statistics
//     pages and the series endpoint behind them now come in.
//   - k: the model queue on the statistics page got agents.vue; the queue,
//     the concurrency limit and the statistics pages now come in.
func TestBriefPlanFollowsWhatThePromptIsAbout(t *testing.T) {
	st := briefGraph(t)
	cases := []struct {
		name, prompt string
		// first are the ranges the plan starts with, in order.
		first []string
		// want are files the plan must reach, before any of notBefore and
		// within its first within ranges when that is set.
		want, notBefore []string
		within          int
		// docsFirst: the documentation leads, and little else follows.
		docsFirst bool
	}{{
		name:      "case h: the documentation site",
		prompt:    "Change the documentation site so every page is marked as draft until verified: update mkdocs.yml, the draft badge hook in docs/hooks/doc_status.py and the docs pages so the status shows on each page.",
		first:     []string{"mkdocs.yml:1-150", "docs/hooks/doc_status.py"},
		want:      []string{"docs/index.md"},
		notBefore: []string{"internal/metrics/metrics.go"},
		docsFirst: true,
	}, {
		name:   "case i: routing in the gateway",
		prompt: "The agent gateway should route local first: only Haiku-class requests qualify for the local model, keep the exclude list, and all cloud models should default to local when they can.",
		want:   []string{"services/api/internal/gateway/local.go", "services/api/internal/api/gateway.go"},
		notBefore: []string{"internal/router/router.go", "services/web/app/components/DonutChart.vue", "services/web/app/pages/settings/organisation.vue",
			"services/api/internal/api/usage_local.go"},
	}, {
		name:   "case j: a statistics tab for the gateway",
		prompt: "the gateway stats should have its own tab/page, a line graph not bar graph of p50, p75, p95, p99 lines to see response time",
		want: []string{"services/web/app/pages/stats/models.vue", "services/web/app/pages/stats/activity.vue",
			"services/api/internal/api/series.go"},
		notBefore: []string{"services/web/app/components/LineChart.vue", "services/web/app/components/ColumnChart.vue",
			"services/web/app/components/DonutChart.vue", "services/web/app/pages/settings/organisation.vue",
			"services/api/internal/api/usage_local.go"},
	}, {
		name:   "case k: the model queue on the statistics page",
		prompt: `something i can see i really mis are "llm-config" queue so when agents are "on-hold/wait" i can't see that anywhere, and i can't see the avgs waiting time ... i need to see that in "peak hours" ... show that as a "week days" and "15m based" in someway in the stats page`,
		// The prompt names the agents, so agents.vue may come in; it got
		// nothing else.
		want:   []string{"services/api/internal/llm/queue.go", "services/api/internal/llm/llm.go", "services/web/app/pages/stats/activity.vue"},
		within: 4,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := briefPlan(t.Context(), st, storetest.Repo, tc.prompt)
			require.NoError(t, err)
			t.Log(plan.Ranges)
			require.True(t, len(plan.Ranges) >= len(tc.first))
			for i, want := range tc.first {
				assert.Equal(t, plan.Ranges[i], want)
			}
			files := make([]string, len(plan.Ranges))
			for i, r := range plan.Ranges {
				files[i] = rangePath(r)
			}
			firstDecoy := len(files)
			for _, f := range tc.notBefore {
				if i := slices.Index(files, f); i >= 0 {
					firstDecoy = min(firstDecoy, i)
				}
			}
			for _, f := range tc.want {
				i := slices.Index(files, f)
				assert.True(t, i >= 0, f+" is not in the plan")
				assert.True(t, i < firstDecoy, f+" comes after "+strings.Join(files[firstDecoy:min(firstDecoy+1, len(files))], ""))
				assert.True(t, tc.within == 0 || i < tc.within, f+" is not among the first ranges")
			}
			if tc.docsFirst {
				// A few other ranges may follow, never lead.
				last := slices.IndexFunc(files, func(f string) bool { return !asklocal.DocsFile(f) })
				assert.True(t, last > 0 && !slices.ContainsFunc(files[last:], asklocal.DocsFile), strings.Join(files, " "))
				assert.True(t, len(files)-last <= maxBackEndRanges, strings.Join(files, " "))
			}
		})
	}
}

func TestRouteResourcesAndAreaOf(t *testing.T) {
	for route, want := range map[string][]string{
		"GET /api/v1/stats/series":                {"stats"},
		"PUT /api/v1/accounts/orgs/{org}/gateway": {"accounts", "gateway"},
		"POST /api/v1/imports/list":               {"imports"},
		"GET /api/v1/repos/{repo}/stats":          {"repos", "stats"},
		"POST /v1/chat/completions":               {"chat"},
	} {
		assert.Equal(t, routeResources(route), want, route)
	}
	areas := []string{"llm", "queue", "stats"}
	assert.Equal(t, areaOf("services/api/internal/llm/queue.go", areas), 1, "the closest area wins")
	assert.Equal(t, areaOf("services/api/internal/llm/llm.go", areas), 0)
	assert.Equal(t, areaOf("services/web/app/pages/stats/activity.vue", areas), 2)
	assert.Equal(t, areaOf("internal/model/model.go", areas), -1)
}
