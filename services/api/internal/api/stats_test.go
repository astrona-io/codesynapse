package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	agentmem "github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/reviews"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/usage"
)

func TestAgentStats(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	got := agentStats([]usage.Bucket{
		{Day: "2026-10-22", Repo: "a", Tool: "get_symbol_context", Client: "claude-code", Calls: 4, ResponseTokens: 1000, DurationMs: 40, MaxDurationMs: 20},
		{Day: "2026-10-22", Repo: "", Tool: "file_outline", Client: "codex", Calls: 2, Errors: 1, ResponseTokens: 100, DurationMs: 10, MaxDurationMs: 10},
		{Day: "2026-10-21", Repo: "a", Tool: "submit_review", Client: "claude-code", Calls: 1, Errors: 1, ResponseTokens: 50},
		{Day: "2026-10-21", Tool: usage.Session, Client: "claude-code", Calls: 2},
		{Day: "2026-09-01", Repo: "a", Tool: "search", Client: "x", Calls: 99}, // outside the window
	}, 7, now, map[string]float64{"a": 11}, 3)

	assert.Equal(t, got.Sessions, 2)
	assert.Equal(t, got.Calls, 7)
	assert.Equal(t, got.Errors, 2)
	assert.Equal(t, got.ResponseTokens, 1150)
	assert.Equal(t, got.AvoidedTokens, 1000*10+100/2*2, "navigation tools only, successful share, per-repo ratio, workspace ratio without a repo")
	require.Len(t, got.Daily, 7)
	assert.Equal(t, got.Daily[6], DayStat{Day: "2026-10-22", Calls: 6, Errors: 1, ResponseTokens: 1100, AvoidedTokens: 10100})
	assert.Equal(t, got.Daily[0].Day, "2026-10-16", "the window is filled with empty days")
	assert.Equal(t, got.Tools[0], ToolStat{Tool: "get_symbol_context", Kind: "navigation", Calls: 4, ResponseTokens: 1000, AvgTokens: 250, AvgMs: 10, MaxMs: 20})
	assert.Equal(t, got.Clients[0], ClientStat{Client: "claude-code", Sessions: 2, Calls: 5})
	assert.Equal(t, toolKind("prompt:code-review"), "prompt")
	assert.Equal(t, toolKind("query_subgraph"), "memory")
}

func TestStatsEndpoint(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	today := usage.Day(time.Now())
	require.NoError(t, st.AddUsage(ctx, []usage.Bucket{
		{Day: today, Tool: usage.Session, Client: "claude-code", Calls: 2},
		{Day: today, Repo: storetest.Repo, Tool: "search_graph_nodes", Client: "claude-code", Calls: 4, ResponseTokens: 800},
		{Day: today, Repo: storetest.Repo, Tool: "upsert_graph_triples", Client: "claude-code", Calls: 1, ResponseTokens: 30},
	}))
	b, err := agentmem.Prepare(storetest.Repo, []agentmem.Triple{{Subject: "Team", Predicate: "OWNS", Object: "api"}}, "claude-code", time.Now())
	require.NoError(t, err)
	require.NoError(t, st.UpsertMemory(ctx, b))
	rv, err := reviews.Prepare(reviews.Review{Repo: storetest.Repo, Title: "T", Verdict: reviews.Comment, Comments: []reviews.CommentItem{{File: "app/app.go", Line: 9, Message: "m"}}}, time.Now())
	require.NoError(t, err)
	require.NoError(t, st.SaveReview(ctx, rv))
	ts := httptest.NewServer(New(ctx, Config{}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)

	var got Stats
	require.Equal(t, get(t, ts, "/api/v1/stats?days=7", &got), 200)
	assert.Equal(t, got.Days, 7)
	assert.Equal(t, got.Agents.Sessions, 2)
	assert.Equal(t, got.Agents.Calls, 5)
	assert.Equal(t, got.Memory.Facts, 1)
	assert.Equal(t, got.Memory.Saves, 1)
	assert.Equal(t, got.Memory.Recalls, 4)
	assert.Equal(t, got.Memory.Model, OffloadInputs{TokensPerFact: 17.8, TokensPerRecall: 200, RecallsPerSession: 2, Measured: true})
	require.Equal(t, len(got.Memory.Daily), 7, "the window is filled with empty days")
	assert.Equal(t, got.Memory.Daily[6], MemoryDay{Day: today, Saves: 1, Recalls: 4})
	assert.Equal(t, got.Memory.Daily[0].Saves, 0)
	assert.Equal(t, got.Memory.Clients, []MemoryClient{{Client: "claude-code", Saves: 1, Recalls: 4}})
	assert.Equal(t, got.Memory.History, 0)
	assert.Equal(t, got.Reviews.Reviews, 1)
	assert.Equal(t, got.Reviews.ByStatus["open"], 1)
	assert.Equal(t, got.Workspace.Repos, 1)
	require.Equal(t, get(t, ts, "/api/v1/stats?days=999", &got), 200)
	assert.Equal(t, got.Days, 30, "out-of-range windows fall back to 30 days")
}

func TestVisibleUsageKeepsCallsWithoutARepository(t *testing.T) {
	sc := scope{
		orgs:  []accounts.Membership{{Org: accounts.Org{ID: "o1"}}},
		reach: map[string]accounts.Reach{"acme/app": {OrgID: "o1"}}, // shared/lib is not reachable
	}
	in := []usage.Bucket{
		{Org: "o1", Repo: "acme/app", Tool: "search", Calls: 1},   // own repository
		{Org: "o1", Repo: "", Tool: usage.Session, Calls: 1},      // own session
		{Org: "o1", Repo: "", Tool: "repo_status", Calls: 1},      // own call, no repo argument
		{Org: "o2", Repo: "", Tool: "repo_status", Calls: 1},      // another organisation's
		{Org: "o2", Repo: "shared/lib", Tool: "search", Calls: 1}, // another organisation's
		{Org: "", Repo: "", Tool: "repo_status", Calls: 1},        // installation token: no organisation
		{Org: "o2", Repo: "acme/app", Tool: "search", Calls: 1},   // another organisation on my repository
	}
	got := visibleUsage(sc, slices.Clone(in))
	var tools []string
	for _, b := range got {
		tools = append(tools, b.Org+"|"+b.Repo+"|"+b.Tool)
	}
	assert.Equal(t, strings.Join(tools, ","), "o1|acme/app|search,o1||(session),o1||repo_status")
	assert.Equal(t, len(visibleUsage(scope{all: true}, slices.Clone(in))), len(in), "single-tenant sees everything")
}
