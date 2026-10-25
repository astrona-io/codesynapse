// Package storetest is the behavioural contract every store.Store must meet.
// Each implementation's tests call Run with a constructor.
package storetest

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"

	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/reviews"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/usage"
	"github.com/astrona-io/codesynapse/internal/vector"
)

// Repo is the repository name used by Fixture.
const Repo = "fixture"

// ID builds a fixture node ID.
func ID(kind model.NodeKind, local string) string { return model.NodeID(Repo, kind, local) }

// FileAnalysis is the analysis record of the fixture's app/app.go.
var FileAnalysis = model.Analysis{Level: model.LevelTyped, Status: model.StatusPartial, Errors: 2, UnresolvedCalls: 1}.Encode()

// Fixture returns a small graph: main -> handler -> (I).Do -> (*impl).Do -> exec.Command,
// plus a test calling handler, a file, a doc section and a commit.
func Fixture() *model.Graph {
	g := model.NewGraph()
	add := func(n model.Node) { n.Repo = Repo; g.AddNode(n) }
	add(model.Node{ID: ID(model.KindRepository, Repo), Kind: model.KindRepository, Name: Repo, Head: "abc", Time: "2026-01-01T00:00:00Z"})
	add(model.Node{ID: ID(model.KindFile, "app/app.go"), Kind: model.KindFile, Name: "app.go", File: "app/app.go", LOC: 40, Churn: 3, Analysis: FileAnalysis})
	add(model.Node{ID: ID(model.KindFile, "app/app_test.go"), Kind: model.KindFile, Name: "app_test.go", File: "app/app_test.go", LOC: 10, IsTest: true})
	add(model.Node{ID: ID(model.KindFunction, "app.main"), Kind: model.KindFunction, Name: "main", QualifiedName: "app.main", File: "app/app.go", StartLine: 3, EndLine: 6, Complexity: 1})
	add(model.Node{ID: ID(model.KindFunction, "app.Handler"), Kind: model.KindFunction, Name: "Handler", QualifiedName: "app.Handler", File: "app/app.go", StartLine: 8, EndLine: 20, Complexity: 7, Exported: true, Summary: "Handler serves."})
	add(model.Node{ID: ID(model.KindInterfaceMethod, "(app.I).Do"), Kind: model.KindInterfaceMethod, Name: "Do", QualifiedName: "(app.I).Do", File: "app/app.go", StartLine: 22, EndLine: 22, Exported: true})
	add(model.Node{ID: ID(model.KindMethod, "(*app.impl).Do"), Kind: model.KindMethod, Name: "Do", QualifiedName: "(*app.impl).Do", File: "app/app.go", StartLine: 25, EndLine: 30, Complexity: 2, Exported: true, SecurityTags: []string{"exec"}})
	add(model.Node{ID: ID(model.KindExternal, "os/exec.Command"), Kind: model.KindExternal, Name: "Command", QualifiedName: "os/exec.Command", SecurityTags: []string{"exec"}})
	add(model.Node{ID: ID(model.KindFunction, "app.TestHandler"), Kind: model.KindFunction, Name: "TestHandler", QualifiedName: "app.TestHandler", File: "app/app_test.go", StartLine: 1, EndLine: 5, IsTest: true, Exported: true})
	add(model.Node{ID: ID(model.KindSection, "README.md#L1"), Kind: model.KindSection, Name: "Usage", File: "README.md", StartLine: 1, EndLine: 9, Summary: "Call Handler."})
	add(model.Node{ID: ID(model.KindDocument, "README.md"), Kind: model.KindDocument, Name: "README", File: "README.md"})
	add(model.Node{ID: ID(model.KindConfig, "deploy/app.yaml"), Kind: model.KindConfig, Name: "app.yaml", File: "deploy/app.yaml"})
	add(model.Node{ID: ID(model.KindConfigEntry, "deploy/app.yaml#Deployment/api"), Kind: model.KindConfigEntry, Name: "Deployment/api", File: "deploy/app.yaml", StartLine: 1, EndLine: 20, Signature: "apps/v1"})
	add(model.Node{ID: ID(model.KindConfigEntry, "deploy/app.yaml#spec"), Kind: model.KindConfigEntry, Name: "spec", File: "deploy/app.yaml", StartLine: 5, EndLine: 20, Signature: "map"})
	add(model.Node{ID: ID(model.KindTemplate, "chart/templates/d.yaml"), Kind: model.KindTemplate, Name: "d.yaml", File: "chart/templates/d.yaml", Package: "chart"})
	add(model.Node{ID: ID(model.KindFile, "web/app.ts"), Kind: model.KindFile, Name: "app.ts", File: "web/app.ts", Language: "typescript"})
	add(model.Node{ID: ID(model.KindCommit, "c1"), Kind: model.KindCommit, Name: "c1", Time: "2026-01-02T00:00:00Z"})
	add(model.Node{ID: ID(model.KindCommit, "c2"), Kind: model.KindCommit, Name: "c2", Time: "2026-01-03T00:00:00Z"})

	edge := func(from, to string, k model.EdgeKind) { g.AddEdge(model.Edge{From: from, To: to, Kind: k}) }
	typed := func(from, to string, k model.EdgeKind) {
		g.AddEdge(model.Edge{From: from, To: to, Kind: k, Evidence: model.EvidenceTyped})
	}
	typed(ID(model.KindFunction, "app.main"), ID(model.KindFunction, "app.Handler"), model.EdgeCalls)
	typed(ID(model.KindFunction, "app.Handler"), ID(model.KindInterfaceMethod, "(app.I).Do"), model.EdgeCalls)
	typed(ID(model.KindInterfaceMethod, "(app.I).Do"), ID(model.KindMethod, "(*app.impl).Do"), model.EdgeDispatchesTo)
	typed(ID(model.KindMethod, "(*app.impl).Do"), ID(model.KindExternal, "os/exec.Command"), model.EdgeCalls)
	// Written before evidence was recorded: must read back as unknown.
	edge(ID(model.KindFunction, "app.TestHandler"), ID(model.KindFunction, "app.Handler"), model.EdgeCalls)
	edge(ID(model.KindFile, "app/app.go"), ID(model.KindFunction, "app.Handler"), model.EdgeDefines)
	edge(ID(model.KindSection, "README.md#L1"), ID(model.KindFunction, "app.Handler"), model.EdgeMentions)
	return g
}

// CodeGraph is the part of a store that holds repositories' code graphs.
type CodeGraph interface {
	store.Reader
	store.Writer
}

// RunCodeGraph exercises only the code graph, for a store that is built in
// parts; Run includes it.
func RunCodeGraph(t *testing.T, s CodeGraph) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.EnsureSchema(ctx))
	require.NoError(t, s.ReplaceRepo(ctx, Repo, Fixture()))
	t.Run("replacement_failure", func(t *testing.T) { replacementContract(t, s) })
	t.Run("successive_replacements", func(t *testing.T) { successiveContract(t, s) })
	t.Run("scoped_graph", func(t *testing.T) { scopedContract(t, s) })
	t.Run("dependency_graph", func(t *testing.T) { dependencyContract(t, s) })
	codeGraphContract(t, s)
	t.Run("finding counts", func(t *testing.T) { findingCountsContract(ctx, t, s) })
	t.Run("delete", func(t *testing.T) { deleteContract(ctx, t, s) })
}

// Records is the part of a store that keeps what people and agents add beside
// the code graph: reviews, snapshots, curated items and usage counters.
type Records interface {
	CodeGraph
	store.ReviewStore
	store.HistoryStore
	store.CuratedStore
	store.UsageStore
	store.MemoryStore
	store.AccessStore
	store.SessionMemoryStore
}

// RunRecords exercises only those records; Run includes them.
func RunRecords(t *testing.T, s Records) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.EnsureSchema(ctx))
	require.NoError(t, s.ReplaceRepo(ctx, Repo, Fixture()))
	t.Run("access_projection", func(t *testing.T) { accessContract(t, s) })
	t.Run("session_memory", func(t *testing.T) { sessionMemoryContract(t, s) })
	t.Run("history", func(t *testing.T) { historyContract(t, s) })
	t.Run("curated", func(t *testing.T) { curatedContract(t, s) })
	t.Run("reviews survive re-indexing", func(t *testing.T) { reviewContract(ctx, t, s) })
	t.Run("usage counters", func(t *testing.T) { usageContract(ctx, t, s) })
	memoryContract(ctx, t, s)
	t.Run("memory_counts", func(t *testing.T) { memoryCountsContract(t, s) })
}

// codeGraphContract reads and replaces the fixture repository.
func codeGraphContract(t *testing.T, s CodeGraph) {
	ctx := context.Background()
	t.Run("repos", func(t *testing.T) {
		repo, ok := findRepo(ctx, t, s)
		require.True(t, ok)
		assert.Equal(t, repo.Head, "abc")
	})

	t.Run("node", func(t *testing.T) {
		n, err := s.Node(ctx, ID(model.KindMethod, "(*app.impl).Do"))
		require.NoError(t, err)
		assert.Equal(t, n.SecurityTags, []string{"exec"})
		assert.Equal(t, n.EndLine, 30)
		_, err = s.Node(ctx, "nope")
		assert.True(t, errors.Is(err, store.ErrNotFound))
	})

	t.Run("search exact first", func(t *testing.T) {
		res, err := s.Search(ctx, Repo, "handler", nil, 10)
		require.NoError(t, err)
		require.Greater(t, len(res), 1)
		assert.Equal(t, res[0].Name, "Handler")
	})

	t.Run("nodes in file ordered", func(t *testing.T) {
		res, err := s.NodesInFile(ctx, Repo, "app/app.go")
		require.NoError(t, err)
		require.Len(t, res, 4)
		assert.Equal(t, res[0].Name, "main")
		assert.Equal(t, res[3].QualifiedName, "(*app.impl).Do")
	})

	t.Run("callers through dispatch", func(t *testing.T) {
		hits, err := s.Traverse(ctx, ID(model.KindMethod, "(*app.impl).Do"),
			[]model.EdgeKind{model.EdgeCalls, model.EdgeDispatchesTo}, store.In, 3, 50)
		require.NoError(t, err)
		depths := map[string]int{}
		for _, h := range hits {
			depths[h.Node.Name] = h.Depth
		}
		assert.Equal(t, depths["Do"], 1)
		assert.Equal(t, depths["Handler"], 2)
		assert.Equal(t, depths["TestHandler"], 3)
		assert.Equal(t, depths["main"], 3)
	})

	t.Run("depth limit", func(t *testing.T) {
		hits, err := s.Traverse(ctx, ID(model.KindMethod, "(*app.impl).Do"),
			[]model.EdgeKind{model.EdgeCalls, model.EdgeDispatchesTo}, store.In, 1, 50)
		require.NoError(t, err)
		assert.Len(t, hits, 1)
	})

	t.Run("callees", func(t *testing.T) {
		hits, err := s.Traverse(ctx, ID(model.KindFunction, "app.main"), []model.EdgeKind{model.EdgeCalls}, store.Out, 1, 50)
		require.NoError(t, err)
		require.Len(t, hits, 1)
		assert.Equal(t, hits[0].Node.Name, "Handler")
	})

	t.Run("list by complexity", func(t *testing.T) {
		res, err := s.List(ctx, Repo, store.Filter{Kinds: []model.NodeKind{model.KindFunction, model.KindMethod}, ExcludeTests: true}, store.ByComplexity, 2)
		require.NoError(t, err)
		require.Len(t, res, 2)
		assert.Equal(t, res[0].Node.Name, "Handler")
		assert.Equal(t, res[0].Score, 7)
	})

	t.Run("list by fan-in", func(t *testing.T) {
		res, err := s.List(ctx, Repo, store.Filter{Kinds: []model.NodeKind{model.KindFunction}}, store.ByFanIn, 1)
		require.NoError(t, err)
		require.Len(t, res, 1)
		assert.Equal(t, res[0].Node.Name, "Handler")
		assert.Equal(t, res[0].Score, 2)
	})

	t.Run("list by tag and undocumented", func(t *testing.T) {
		res, err := s.List(ctx, Repo, store.Filter{Tag: "exec", Kinds: []model.NodeKind{model.KindMethod}, ExportedOnly: true, Undocumented: true}, store.ByName, 10)
		require.NoError(t, err)
		require.Len(t, res, 1)
		assert.Equal(t, res[0].Node.QualifiedName, "(*app.impl).Do")
	})

	t.Run("list commits by time", func(t *testing.T) {
		res, err := s.List(ctx, Repo, store.Filter{Kinds: []model.NodeKind{model.KindCommit}}, store.ByTime, 10)
		require.NoError(t, err)
		require.Len(t, res, 2)
		assert.Equal(t, res[0].Node.Name, "c2")
	})

	t.Run("subgraph", func(t *testing.T) {
		ids := []string{ID(model.KindFunction, "app.main"), ID(model.KindFunction, "app.Handler"), ID(model.KindFile, "app/app.go")}
		edges, err := s.Subgraph(ctx, ids, nil)
		require.NoError(t, err)
		assert.Len(t, edges, 2)
		edges, err = s.Subgraph(ctx, ids, []model.EdgeKind{model.EdgeCalls})
		require.NoError(t, err)
		require.Len(t, edges, 1)
		assert.Equal(t, edges[0].Count, 1)
		assert.Equal(t, edges[0].Evidence, model.EvidenceTyped)
	})

	t.Run("edge evidence and analysis", func(t *testing.T) {
		edges, err := s.Subgraph(ctx, []string{ID(model.KindFunction, "app.TestHandler"), ID(model.KindFunction, "app.Handler")}, nil)
		require.NoError(t, err)
		require.Len(t, edges, 1)
		assert.Equal(t, edges[0].Evidence, model.Evidence(""), "edges without evidence stay unknown")
		edges, err = s.Subgraph(ctx, []string{ID(model.KindInterfaceMethod, "(app.I).Do"), ID(model.KindMethod, "(*app.impl).Do")}, nil)
		require.NoError(t, err)
		require.Len(t, edges, 1)
		assert.Equal(t, edges[0].Evidence, model.EvidenceTyped)
		n, err := s.Node(ctx, ID(model.KindFile, "app/app.go"))
		require.NoError(t, err)
		assert.Equal(t, n.Analysis, FileAnalysis)
	})

	t.Run("stats", func(t *testing.T) {
		st, err := s.Stats(ctx, Repo)
		require.NoError(t, err)
		assert.Equal(t, st.Nodes[model.KindFunction], 3)
		assert.Equal(t, st.Edges[model.EdgeCalls], 4)
		assert.Equal(t, st.Tests, 1)
		assert.Equal(t, st.Exported, 2)
		assert.Equal(t, st.Documented, 1)
		assert.Equal(t, st.SecurityTags["exec"], 1)
		assert.Equal(t, st.TotalLOC, 50)
		assert.InDelta(t, st.AvgComplexity, 10.0/3.0, 0.01)
		assert.Equal(t, st.Formats["go"], 2)
		assert.Equal(t, st.Formats["typescript"], 1)
		assert.Equal(t, st.Formats["yaml"], 1)
		assert.Equal(t, st.Formats["helm"], 1)
		assert.Equal(t, st.Formats["markdown"], 1)
		assert.Equal(t, st.K8sResources, 1)
		assert.Equal(t, st.HelmCharts, 1)
	})

	t.Run("replace removes stale", func(t *testing.T) {
		g := Fixture()
		small := model.NewGraph()
		for _, n := range g.Nodes() {
			if n.Kind != model.KindCommit {
				small.AddNode(n)
			}
		}
		for _, e := range g.Edges() {
			small.AddEdge(e)
		}
		require.NoError(t, s.ReplaceRepo(ctx, Repo, small))
		res, err := s.List(ctx, Repo, store.Filter{Kinds: []model.NodeKind{model.KindCommit}}, store.ByTime, 10)
		require.NoError(t, err)
		assert.Len(t, res, 0)
	})

}

// Run exercises s against the contract. s may hold other repositories;
// only the fixture repo is written and checked.
func Run(t *testing.T, s store.Store) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.EnsureSchema(ctx))
	require.NoError(t, s.ReplaceRepo(ctx, Repo, Fixture()))
	t.Run("replacement_failure", func(t *testing.T) { replacementContract(t, s) })
	t.Run("successive_replacements", func(t *testing.T) { successiveContract(t, s) })
	t.Run("access_projection", func(t *testing.T) { accessContract(t, s) })
	t.Run("scoped_graph", func(t *testing.T) { scopedContract(t, s) })
	t.Run("dependency_graph", func(t *testing.T) { dependencyContract(t, s) })
	t.Run("history", func(t *testing.T) { historyContract(t, s) })
	t.Run("curated", func(t *testing.T) { curatedContract(t, s) })
	t.Run("identity", func(t *testing.T) { identityContract(t, s) })
	t.Run("memory_counts", func(t *testing.T) { memoryCountsContract(t, s) })
	t.Run("session_memory", func(t *testing.T) {
		sm, ok := s.(interface {
			store.SessionMemoryStore
			store.MemoryStore
		})
		require.True(t, ok)
		sessionMemoryContract(t, sm)
	})
	t.Run("runtime_facts", func(t *testing.T) { runtimeContract(t, s) })

	codeGraphContract(t, s)

	t.Run("reviews survive re-indexing", func(t *testing.T) { reviewContract(ctx, t, s) })

	memoryContract(ctx, t, s)

	t.Run("usage counters", func(t *testing.T) { usageContract(ctx, t, s) })

	t.Run("finding counts", func(t *testing.T) { findingCountsContract(ctx, t, s) })
	t.Run("delete", func(t *testing.T) { deleteContract(ctx, t, s) })
}

func findingCountsContract(ctx context.Context, t *testing.T, s CodeGraph) {
	require.NoError(t, s.ReplaceRepo(ctx, Repo, Fixture()))
	got, err := s.FindingCounts(ctx, Repo)
	require.NoError(t, err)
	want := map[string]int{}
	for _, n := range Fixture().Nodes() {
		if n.Kind == model.KindFinding {
			want[n.Severity]++
		}
	}
	assert.Equal(t, got, want)
}

func deleteContract(ctx context.Context, t *testing.T, s CodeGraph) {
	require.NoError(t, s.DeleteRepo(ctx, Repo))
	_, ok := findRepo(ctx, t, s)
	assert.False(t, ok)
}

func findRepo(ctx context.Context, t *testing.T, s store.Reader) (model.Node, bool) {
	t.Helper()
	repos, err := s.Repos(ctx)
	require.NoError(t, err)
	for _, r := range repos {
		if r.Name == Repo {
			return r, true
		}
	}
	return model.Node{}, false
}

// findEntity reports whether repo's memory has an entity with this exact
// normalised ID name.
func findEntity(ctx context.Context, s store.MemoryStore, repo, name string) (memory.Entity, bool) {
	g, err := s.SearchMemory(ctx, repo, "", store.MaxLimit, true)
	if err != nil {
		return memory.Entity{}, false
	}
	for _, e := range g.Entities {
		if e.ID == memory.EntityID(repo, name) {
			return e, true
		}
	}
	return memory.Entity{}, false
}

func reviewContract(ctx context.Context, t *testing.T, s interface {
	store.Writer
	store.ReviewStore
}) {
	r, err := reviews.Prepare(reviews.Review{Repo: Repo, Title: "Review", Comments: []reviews.CommentItem{{File: "app/app.go", Line: 9, Message: "m"}}}, time.Now())
	require.NoError(t, err)
	require.NoError(t, s.SaveReview(ctx, r))
	require.NoError(t, s.ReplaceRepo(ctx, Repo, Fixture()))
	got, err := s.GetReview(ctx, r.ID)
	require.NoError(t, err)
	assert.Equal(t, got.Comments[0].Message, "m")
	list, err := s.ListReviews(ctx, Repo, 10)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.NoError(t, reviews.ApplyStatus(&got, got.Comments[0].ID, reviews.Resolved, "done", "test", time.Now()))
	require.NoError(t, s.SaveReview(ctx, got))
	again, err := s.GetReview(ctx, r.ID)
	require.NoError(t, err)
	assert.Equal(t, again.Comments[0].Status, reviews.Resolved)
	require.NoError(t, s.DeleteReview(ctx, r.ID))
	_, err = s.GetReview(ctx, r.ID)
	assert.True(t, errors.Is(err, store.ErrNotFound))
}

func usageContract(ctx context.Context, t *testing.T, s store.UsageStore) {
	// Days in 2000 keep the integration run away from real counters.
	t.Cleanup(func() { _ = s.PruneUsage(ctx, "2000-01-03") })
	b := usage.Bucket{Day: "2000-01-01", Repo: Repo, Tool: "search", Client: "claude-code", Calls: 2, Errors: 1, ResponseTokens: 300, DurationMs: 40, MaxDurationMs: 30}
	require.NoError(t, s.AddUsage(ctx, []usage.Bucket{b}))
	require.NoError(t, s.AddUsage(ctx, []usage.Bucket{{Day: "2000-01-01", Repo: Repo, Tool: "search", Client: "claude-code", Calls: 1, ResponseTokens: 100, DurationMs: 50, MaxDurationMs: 50},
		{Day: "2000-01-02", Repo: Repo, Tool: usage.Session, Client: "codex", Calls: 1}}))
	// Hook reports add only the range counters to the same bucket.
	require.NoError(t, s.AddUsage(ctx, []usage.Bucket{{Day: "2000-01-01", Repo: Repo, Tool: "search", Client: "claude-code", RangesReturned: 5, RangesRead: 2}}))
	got, err := s.Usage(ctx, "2000-01-01")
	require.NoError(t, err)
	var mine []usage.Bucket
	for _, g := range got {
		if g.Day < "2000-01-03" {
			mine = append(mine, g)
		}
	}
	require.Len(t, mine, 2)
	assert.Equal(t, mine[0], usage.Bucket{Day: "2000-01-01", Repo: Repo, Tool: "search", Client: "claude-code", Calls: 3, Errors: 1, ResponseTokens: 400, DurationMs: 90, MaxDurationMs: 50,
		RangesReturned: 5, RangesRead: 2})
	assert.Equal(t, mine[1].Tool, usage.Session)
	require.NoError(t, s.PruneUsage(ctx, "2000-01-02"))
	got, err = s.Usage(ctx, "2000-01-01")
	require.NoError(t, err)
	assert.Equal(t, got[0].Day, "2000-01-02", "older days pruned")
}

// memoryContract is agent memory: triples, aliases, history, vectors,
// namespaces, lexical ranking and statistics.
func memoryContract(ctx context.Context, t *testing.T, s interface {
	store.Writer
	store.MemoryStore
}) {
	t.Run("agent memory", func(t *testing.T) {
		t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		// The integration run uses a live database: leave no memory behind.
		t.Cleanup(func() {
			for _, repo := range []string{Repo, "other-repo"} {
				g, err := s.SearchMemory(ctx, repo, "", store.MaxLimit, false)
				if err != nil {
					continue
				}
				for _, e := range g.Entities {
					_ = s.DeleteMemoryEntity(ctx, repo, e.ID)
				}
			}
		})
		save := func(at time.Time, triples ...memory.Triple) {
			t.Helper()
			b, err := memory.Prepare(Repo, triples, "test-agent", at)
			require.NoError(t, err)
			require.NoError(t, s.UpsertMemory(ctx, b))
		}
		save(t0,
			memory.Triple{Subject: "User", SubjectType: "Person", Predicate: "prefers", Object: "Dark mode", ObjectType: "Preference"},
			memory.Triple{Subject: "Project Alpha", SubjectType: "Project", Predicate: "uses framework", Object: "FastAPI", ObjectType: "Framework"},
		)
		save(t0.Add(time.Hour), memory.Triple{Subject: "project alpha", Predicate: "USES_FRAMEWORK", Object: "fastapi"}) // same fact, other case
		save(t0.Add(2*time.Hour), memory.Triple{Subject: "User", Predicate: "PREFERS", Object: "Light mode", Replace: true})
		other, err := memory.Prepare("other-repo", []memory.Triple{{Subject: "User", Predicate: "PREFERS", Object: "Vim"}}, "x", t0)
		require.NoError(t, err)
		require.NoError(t, s.UpsertMemory(ctx, other))

		g, err := s.SearchMemory(ctx, Repo, "project alpha", 10, false)
		require.NoError(t, err)
		assert.Equal(t, g.Notice, memory.Notice)
		require.Len(t, g.Entities, 2)
		assert.Equal(t, g.Entities[0].Name, "project alpha", "matched first; the latest spelling wins")
		assert.Equal(t, g.Entities[0].Type, "Project", "a later save without a type keeps it")
		assert.Equal(t, g.Entities[1].Name, "fastapi", "then the other end of its facts")
		require.Len(t, g.Facts, 1)
		assert.Equal(t, g.Facts[0].Predicate, "USES_FRAMEWORK")
		assert.Equal(t, g.Facts[0].Confirmations, 2)
		assert.Equal(t, g.Facts[0].CreatedAt, "2026-01-01T00:00:00Z")
		assert.Equal(t, g.Facts[0].Source, "test-agent")

		g, err = s.SearchMemory(ctx, Repo, "user", 10, false)
		require.NoError(t, err)
		require.Len(t, g.Facts, 1, "replace closed Dark mode; other-repo facts are separate")
		assert.Equal(t, g.Facts[0].Object, memory.EntityID(Repo, "Light mode"))
		assert.Equal(t, g.Facts[0].ValidFrom, "2026-01-01T02:00:00Z")
		hist, err := s.SearchMemory(ctx, Repo, "user", 10, true)
		require.NoError(t, err)
		require.Len(t, hist.Facts, 2, "the replaced fact is kept as history")
		for _, f := range hist.Facts {
			if f.Object == memory.EntityID(Repo, "Dark mode") {
				assert.Equal(t, f.ValidTo, "2026-01-01T02:00:00Z")
			}
		}

		all, err := s.SearchMemory(ctx, Repo, "", 2, false)
		require.NoError(t, err)
		require.True(t, len(all.Entities) >= 2)
		assert.Equal(t, all.Entities[0].Name, "Light mode", "empty query: most recently updated first")

		require.NoError(t, s.ReplaceRepo(ctx, Repo, Fixture()))
		sub, err := s.MemorySubgraph(ctx, Repo, []string{memory.EntityID(Repo, "fastapi")}, 1, 10, false)
		require.NoError(t, err)
		require.Len(t, sub.Facts, 1, "memory survives re-indexing")
		assert.Len(t, sub.Entities, 2)

		require.NoError(t, s.DeleteMemoryFact(ctx, Repo, sub.Facts[0].ID))
		root := memory.EntityID(Repo, "fastapi")
		isolated, err := s.MemorySubgraph(ctx, Repo, []string{root, root, memory.EntityID("other-repo", "fastapi"), "missing"}, 2, 10, false)
		require.NoError(t, err)
		require.Len(t, isolated.Entities, 1, "retain existing roots without facts; reject foreign/missing roots and duplicates")
		assert.Equal(t, isolated.Entities[0].ID, root)
		assert.Len(t, isolated.Facts, 0)
		_, err = s.MemorySubgraph(ctx, Repo, make([]string, store.MaxLimit+1), 1, 10, false)
		assert.Error(t, err, "root lists are bounded")
		assert.True(t, errors.Is(s.DeleteMemoryFact(ctx, Repo, sub.Facts[0].ID), store.ErrNotFound))
		require.NoError(t, s.DeleteMemoryEntity(ctx, Repo, memory.EntityID(Repo, "User")))
		g, err = s.SearchMemory(ctx, Repo, "user light", 10, false)
		require.NoError(t, err)
		assert.Len(t, g.Facts, 0, "deleting an entity deletes its facts")
		assert.True(t, errors.Is(s.DeleteMemoryEntity(ctx, "other-repo", memory.EntityID(Repo, "User")), store.ErrNotFound), "repositories are isolated")
	})

	t.Run("memory aliases and history", func(t *testing.T) {
		t.Cleanup(func() {
			if g, err := s.SearchMemory(ctx, Repo, "", store.MaxLimit, true); err == nil {
				for _, e := range g.Entities {
					_ = s.DeleteMemoryEntity(ctx, Repo, e.ID)
				}
			}
		})
		t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		save := func(at time.Time, triples ...memory.Triple) {
			t.Helper()
			b, err := memory.Prepare(Repo, triples, "agent", at)
			require.NoError(t, err)
			require.NoError(t, s.UpsertMemory(ctx, b))
		}
		// "me" and "I" are the speaker; User_1 starts as a separate entity.
		save(t0, memory.Triple{Subject: "me", Predicate: "PREFERS", Object: "Tabs", Confidence: 0.6},
			memory.Triple{Subject: "User_1", Predicate: "OWNS", Object: "billing"})
		save(t0.Add(time.Hour), memory.Triple{Subject: "I", Predicate: "PREFERS", Object: "tabs", Confidence: 0.9})
		// Declaring User_1 the same as the user merges it, facts included.
		save(t0.Add(2*time.Hour), memory.Triple{Subject: "User_1", Predicate: "SAME_AS", Object: "User"})
		// Later saves under the alias land on User.
		save(t0.Add(3*time.Hour), memory.Triple{Subject: "user_1", Predicate: "REVIEWS", Object: "billing"})
		// A fact and the alias for its subject in one call: the fact lands on the canonical entity.
		save(t0.Add(3*time.Hour), memory.Triple{Subject: "Bob_2", Predicate: "MAINTAINS", Object: "billing"},
			memory.Triple{Subject: "Bob_2", Predicate: "SAME_AS", Object: "Bob"})
		bob, err := s.SearchMemory(ctx, Repo, "bob", 10, false)
		require.NoError(t, err)
		require.Len(t, bob.Facts, 1, "the fact survives its subject being declared an alias")
		assert.Equal(t, bob.Facts[0].Subject, memory.EntityID(Repo, "Bob"))

		g, err := s.SearchMemory(ctx, Repo, "user_1", 10, false)
		require.NoError(t, err)
		require.True(t, len(g.Entities) >= 1)
		assert.Equal(t, g.Entities[0].Name, memory.Speaker, "found through its alias")
		assert.Equal(t, g.Entities[0].Aliases, []string{"user_1"})
		preds := map[string]memory.Fact{}
		for _, f := range g.Facts {
			assert.Equal(t, f.Subject, memory.EntityID(Repo, memory.Speaker))
			preds[f.Predicate] = f
		}
		assert.Equal(t, len(preds), 3, "PREFERS, the merged OWNS and REVIEWS")
		assert.Equal(t, preds["PREFERS"].Confirmations, 2)
		assert.Equal(t, preds["PREFERS"].Confidence, 0.9, "confidence keeps the maximum")
		_, gone := findEntity(ctx, s, Repo, "user_1")
		assert.False(t, gone, "the duplicate entity was merged away")

		// Replace, then save the old value again: it reopens.
		save(t0.Add(4*time.Hour), memory.Triple{Subject: "User", Predicate: "PREFERS", Object: "Spaces", Replace: true})
		save(t0.Add(5*time.Hour), memory.Triple{Subject: "User", Predicate: "PREFERS", Object: "Tabs", Replace: true})
		h, err := s.SearchMemory(ctx, Repo, "user", 10, true)
		require.NoError(t, err)
		for _, f := range h.Facts {
			switch f.Object {
			case memory.EntityID(Repo, "Tabs"):
				assert.True(t, f.Current())
				assert.Equal(t, f.ValidFrom, "2026-03-01T05:00:00Z", "reopened facts are valid from the new save")
			case memory.EntityID(Repo, "Spaces"):
				assert.Equal(t, f.ValidTo, "2026-03-01T05:00:00Z")
			}
		}
	})

	t.Run("memory vectors", func(t *testing.T) {
		t.Cleanup(func() {
			for _, n := range []string{"payments", "billing", "frontend"} {
				_ = s.DeleteMemoryEntity(ctx, Repo, memory.EntityID(Repo, n))
			}
		})
		b, err := memory.Prepare(Repo, []memory.Triple{
			{Subject: "payments", Predicate: "DEPENDS_ON", Object: "billing"},
			{Subject: "frontend", Predicate: "CALLS", Object: "payments"},
		}, "agent", time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))
		require.NoError(t, err)
		require.NoError(t, s.UpsertMemory(ctx, b))

		todo, err := s.MemoryEmbeddingTodo(ctx, "m1", 500)
		require.NoError(t, err)
		mine := map[string]bool{}
		for _, e := range todo {
			if e.Repo == Repo {
				mine[e.Name] = true
			}
		}
		assert.Equal(t, len(mine), 3, "nothing is embedded yet")

		id := func(n string) string { return memory.EntityID(Repo, n) }
		require.Error(t, s.SetMemoryEmbeddings(ctx, "m1", map[string][]float32{id("payments"): {1, 0, 0}, id("billing"): {1, 0}}), "lengths must match")
		require.NoError(t, s.SetMemoryEmbeddings(ctx, "m1", map[string][]float32{
			id("payments"): {1, 0, 0}, id("billing"): {0.8, 0.6, 0}, id("frontend"): {0, 0, 1},
		}))
		near, err := s.MemoryNearest(ctx, Repo, "m1", []float32{0.9, 0.1, 0}, 2)
		require.NoError(t, err)
		require.Len(t, near, vector.MinK, "a search returns at least MinK hits")
		assert.Equal(t, near[0].Name, "payments")
		assert.Equal(t, near[1].Name, "billing")
		assert.Equal(t, near[2].Name, "frontend")
		assert.Equal(t, math.Round(near[0].Score*1000)/1000, math.Round(memory.Similarity([]float32{0.9, 0.1, 0}, []float32{1, 0, 0})*1000)/1000, "(1+cos)/2 in every store")
		other, err := s.MemoryNearest(ctx, Repo, "m2", []float32{0.9, 0.1, 0}, 2)
		require.NoError(t, err)
		assert.Equal(t, len(other), 0, "vectors of another model are not compared")
		todo, err = s.MemoryEmbeddingTodo(ctx, "m1", 500)
		require.NoError(t, err)
		for _, e := range todo {
			assert.False(t, e.Repo == Repo, "embedded entities are done: "+e.Name)
		}
	})

	t.Run("memory per organisation", func(t *testing.T) {
		a, b := memory.Namespace("org-a", Repo), memory.Namespace("org-b", Repo)
		t.Cleanup(func() {
			for _, ns := range []string{a, b} {
				_ = s.DeleteMemoryEntity(ctx, ns, memory.EntityID(ns, "payments"))
				_ = s.DeleteMemoryEntity(ctx, ns, memory.EntityID(ns, "team blue"))
			}
		})
		batch, err := memory.Prepare(a, []memory.Triple{{Subject: "Payments", Predicate: "OWNED_BY", Object: "Team Blue"}}, "agent",
			time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
		require.NoError(t, err)
		require.NoError(t, s.UpsertMemory(ctx, batch))

		g, err := s.SearchMemory(ctx, a, "payments", 10, false)
		require.NoError(t, err)
		require.Len(t, g.Facts, 1)
		factID := g.Facts[0].ID
		g, err = s.SearchMemory(ctx, b, "payments", 10, false)
		require.NoError(t, err)
		assert.Len(t, g.Entities, 0, "another organisation on the same graph sees none of it")
		sub, err := s.MemorySubgraph(ctx, b, []string{memory.EntityID(a, "payments")}, 2, 10, true)
		require.NoError(t, err)
		assert.Len(t, sub.Facts, 0, "naming another namespace's entity reaches nothing")
		assert.ErrorIs(t, s.DeleteMemoryEntity(ctx, b, memory.EntityID(a, "payments")), store.ErrNotFound)
		assert.ErrorIs(t, s.DeleteMemoryFact(ctx, b, factID), store.ErrNotFound)

		require.NoError(t, s.SetMemoryEmbeddings(ctx, "m-org", map[string][]float32{memory.EntityID(a, "payments"): {1, 0}}))
		near, err := s.MemoryNearest(ctx, b, "m-org", []float32{1, 0}, 5)
		require.NoError(t, err)
		assert.Len(t, near, 0, "vectors stay within their organisation")
	})

	t.Run("memory lexical ranking", func(t *testing.T) {
		a, b := memory.Namespace("org-lex", Repo), memory.Namespace("org-other", Repo)
		t.Cleanup(func() {
			for _, ns := range []string{a, b} {
				for _, n := range []string{"payments service", "billing service", "search service", "frontend", "team web", "checkouthandler", "team blue"} {
					_ = s.DeleteMemoryEntity(ctx, ns, memory.EntityID(ns, n))
				}
			}
		})
		at := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
		batch, err := memory.Prepare(a, []memory.Triple{
			{Subject: "Payments service", Predicate: "OWNED_BY", Object: "Team Blue"},
			{Subject: "Billing service", Predicate: "OWNED_BY", Object: "Team Blue"},
			{Subject: "Search service", Predicate: "OWNED_BY", Object: "Team Blue"},
			{Subject: "Frontend", Predicate: "OWNED_BY", Object: "Team Web"},
			{Subject: "CheckoutHandler", SubjectType: "Component", Predicate: "CALLS", Object: "Payments service"},
		}, "agent", at)
		require.NoError(t, err)
		require.NoError(t, s.UpsertMemory(ctx, batch))
		other, err := memory.Prepare(b, []memory.Triple{{Subject: "Payments service", Predicate: "OWNED_BY", Object: "Team Blue"}}, "agent", at)
		require.NoError(t, err)
		require.NoError(t, s.UpsertMemory(ctx, other))

		got, err := s.MemoryLexical(ctx, a, "payments service", 8)
		require.NoError(t, err)
		require.True(t, len(got) >= 3, "%v", got)
		assert.Equal(t, got[0].Name, "Payments service", "the rare term decides")
		for _, e := range got {
			assert.Equal(t, e.Repo, a, "only the namespace's entities")
		}
		got, err = s.MemoryLexical(ctx, a, "checkout handler", 8)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, got[0].Name, "CheckoutHandler")
		got, err = s.MemoryLexical(ctx, a, "service", 1)
		require.NoError(t, err)
		assert.Len(t, got, vector.MinK, "k is clamped like vector search")
		got, err = s.MemoryLexical(ctx, a, "the", 8)
		require.NoError(t, err)
		assert.Len(t, got, 0, "stopwords match nothing")
		got, err = s.MemoryLexical(ctx, memory.Namespace("org-none", Repo), "payments", 8)
		require.NoError(t, err)
		assert.Len(t, got, 0, "another organisation's namespace is empty")
	})

	t.Run("memory stats", func(t *testing.T) {
		t.Cleanup(func() {
			for _, name := range []string{"Team", "api"} {
				_ = s.DeleteMemoryEntity(ctx, Repo, memory.EntityID(Repo, name))
			}
		})
		b, err := memory.Prepare(Repo, []memory.Triple{{Subject: "Team", Predicate: "OWNS", Object: "api"}}, "agent-a", time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
		require.NoError(t, err)
		require.NoError(t, s.UpsertMemory(ctx, b))
		require.NoError(t, s.UpsertMemory(ctx, b))
		stats, err := s.MemoryStats(ctx)
		require.NoError(t, err)
		var got memory.Stats
		for _, st := range stats {
			if st.Repo == Repo {
				got = st
			}
		}
		assert.Equal(t, got.Entities, 2)
		assert.Equal(t, got.Facts, 1)
		assert.Equal(t, got.Confirmations, 2)
		assert.Equal(t, got.FactChars, len("Team")+len("OWNS")+len("api"))
		assert.Equal(t, got.Sources, map[string]int{"agent-a": 1})
		assert.Equal(t, got.LastUpdated, "2026-02-01T00:00:00Z")
	})
}
