// Package storetest is the behavioural contract every store.Store must meet.
// Each implementation's tests call Run with a constructor.
package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"

	"github.com/parisnakitakejser/codesynapse/internal/memory"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/reviews"
	"github.com/parisnakitakejser/codesynapse/internal/store"
	"github.com/parisnakitakejser/codesynapse/internal/usage"
)

// Repo is the repository name used by Fixture.
const Repo = "fixture"

// ID builds a fixture node ID.
func ID(kind model.NodeKind, local string) string { return model.NodeID(Repo, kind, local) }

// Fixture returns a small graph: main -> handler -> (I).Do -> (*impl).Do -> exec.Command,
// plus a test calling handler, a file, a doc section and a commit.
func Fixture() *model.Graph {
	g := model.NewGraph()
	add := func(n model.Node) { n.Repo = Repo; g.AddNode(n) }
	add(model.Node{ID: ID(model.KindRepository, Repo), Kind: model.KindRepository, Name: Repo, Head: "abc", Time: "2026-01-01T00:00:00Z"})
	add(model.Node{ID: ID(model.KindFile, "app/app.go"), Kind: model.KindFile, Name: "app.go", File: "app/app.go", LOC: 40, Churn: 3})
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
	edge(ID(model.KindFunction, "app.main"), ID(model.KindFunction, "app.Handler"), model.EdgeCalls)
	edge(ID(model.KindFunction, "app.Handler"), ID(model.KindInterfaceMethod, "(app.I).Do"), model.EdgeCalls)
	edge(ID(model.KindInterfaceMethod, "(app.I).Do"), ID(model.KindMethod, "(*app.impl).Do"), model.EdgeDispatchesTo)
	edge(ID(model.KindMethod, "(*app.impl).Do"), ID(model.KindExternal, "os/exec.Command"), model.EdgeCalls)
	edge(ID(model.KindFunction, "app.TestHandler"), ID(model.KindFunction, "app.Handler"), model.EdgeCalls)
	edge(ID(model.KindFile, "app/app.go"), ID(model.KindFunction, "app.Handler"), model.EdgeDefines)
	edge(ID(model.KindSection, "README.md#L1"), ID(model.KindFunction, "app.Handler"), model.EdgeMentions)
	return g
}

// Run exercises s against the contract. s may hold other repositories;
// only the fixture repo is written and checked.
func Run(t *testing.T, s store.Store) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.EnsureSchema(ctx))
	require.NoError(t, s.ReplaceRepo(ctx, Repo, Fixture()))

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

	t.Run("reviews survive re-indexing", func(t *testing.T) {
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
	})

	t.Run("agent memory", func(t *testing.T) {
		t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		// The integration run uses a live database: leave no memory behind.
		t.Cleanup(func() {
			for _, repo := range []string{Repo, "other-repo"} {
				g, err := s.SearchMemory(ctx, repo, "", store.MaxLimit)
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

		g, err := s.SearchMemory(ctx, Repo, "project alpha", 10)
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

		g, err = s.SearchMemory(ctx, Repo, "user", 10)
		require.NoError(t, err)
		require.Len(t, g.Facts, 1, "replace removed Dark mode; other-repo facts are separate")
		assert.Equal(t, g.Facts[0].Object, memory.EntityID(Repo, "Light mode"))

		all, err := s.SearchMemory(ctx, Repo, "", 2)
		require.NoError(t, err)
		require.True(t, len(all.Entities) >= 2)
		assert.Equal(t, all.Entities[0].Name, "Light mode", "empty query: most recently updated first")

		require.NoError(t, s.ReplaceRepo(ctx, Repo, Fixture()))
		sub, err := s.MemorySubgraph(ctx, Repo, []string{memory.EntityID(Repo, "fastapi")}, 1, 10)
		require.NoError(t, err)
		require.Len(t, sub.Facts, 1, "memory survives re-indexing")
		assert.Len(t, sub.Entities, 2)

		require.NoError(t, s.DeleteMemoryFact(ctx, Repo, sub.Facts[0].ID))
		assert.True(t, errors.Is(s.DeleteMemoryFact(ctx, Repo, sub.Facts[0].ID), store.ErrNotFound))
		require.NoError(t, s.DeleteMemoryEntity(ctx, Repo, memory.EntityID(Repo, "User")))
		g, err = s.SearchMemory(ctx, Repo, "user light", 10)
		require.NoError(t, err)
		assert.Len(t, g.Facts, 0, "deleting an entity deletes its facts")
		assert.True(t, errors.Is(s.DeleteMemoryEntity(ctx, "other-repo", memory.EntityID(Repo, "User")), store.ErrNotFound), "repositories are isolated")
	})

	t.Run("memory stats", func(t *testing.T) {
		t.Cleanup(func() { _ = s.DeleteMemoryEntity(ctx, Repo, memory.EntityID(Repo, "Team")) })
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

	t.Run("usage counters", func(t *testing.T) {
		// Days in 2000 keep the integration run away from real counters.
		t.Cleanup(func() { _ = s.PruneUsage(ctx, "2000-01-03") })
		b := usage.Bucket{Day: "2000-01-01", Repo: Repo, Tool: "search", Client: "claude-code", Calls: 2, Errors: 1, ResponseTokens: 300, DurationMs: 40, MaxDurationMs: 30}
		require.NoError(t, s.AddUsage(ctx, []usage.Bucket{b}))
		require.NoError(t, s.AddUsage(ctx, []usage.Bucket{{Day: "2000-01-01", Repo: Repo, Tool: "search", Client: "claude-code", Calls: 1, ResponseTokens: 100, DurationMs: 50, MaxDurationMs: 50},
			{Day: "2000-01-02", Repo: Repo, Tool: usage.Session, Client: "codex", Calls: 1}}))
		got, err := s.Usage(ctx, "2000-01-01")
		require.NoError(t, err)
		var mine []usage.Bucket
		for _, g := range got {
			if g.Day < "2000-01-03" {
				mine = append(mine, g)
			}
		}
		require.Len(t, mine, 2)
		assert.Equal(t, mine[0], usage.Bucket{Day: "2000-01-01", Repo: Repo, Tool: "search", Client: "claude-code", Calls: 3, Errors: 1, ResponseTokens: 400, DurationMs: 90, MaxDurationMs: 50})
		assert.Equal(t, mine[1].Tool, usage.Session)
		require.NoError(t, s.PruneUsage(ctx, "2000-01-02"))
		got, err = s.Usage(ctx, "2000-01-01")
		require.NoError(t, err)
		assert.Equal(t, got[0].Day, "2000-01-02", "older days pruned")
	})

	t.Run("delete", func(t *testing.T) {
		require.NoError(t, s.DeleteRepo(ctx, Repo))
		_, ok := findRepo(ctx, t, s)
		assert.False(t, ok)
	})
}

func findRepo(ctx context.Context, t *testing.T, s store.Store) (model.Node, bool) {
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
