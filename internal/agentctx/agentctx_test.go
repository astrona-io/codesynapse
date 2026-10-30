package agentctx

import (
	"context"
	"errors"
	"testing"

	"github.com/astrona-io/codesynapse/internal/duplicates"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func svc(t *testing.T) Service {
	t.Helper()
	g := storetest.Fixture()
	add := func(n model.Node) { n.Repo = storetest.Repo; g.AddNode(n) }
	pkg := storetest.ID(model.KindPackage, "example.com/app")
	add(model.Node{ID: pkg, Kind: model.KindPackage, Name: "app", QualifiedName: "example.com/app"})
	g.AddEdge(model.Edge{From: pkg, To: storetest.ID(model.KindFile, "app/app.go"), Kind: model.EdgeContains})
	s := memory.New()
	require.NoError(t, s.ReplaceRepo(context.Background(), storetest.Repo, g))
	return Service{R: s}
}

func TestResolve(t *testing.T) {
	s := svc(t)
	ctx := context.Background()
	tests := []struct {
		in, want string
	}{
		{"app.Handler", "app.Handler"},
		{"Handler", "app.Handler"},
		{"(*app.impl).Do", "(*app.impl).Do"},
		{storetest.ID(model.KindFunction, "app.main"), "app.main"},
		{"example.com/app", "example.com/app"},
	}
	for _, tt := range tests {
		n, err := s.Resolve(ctx, storetest.Repo, tt.in)
		require.NoError(t, err, tt.in)
		assert.Equal(t, n.ShortName(), tt.want)
	}
	_, err := s.Resolve(ctx, storetest.Repo, "Do")
	assert.True(t, IsSuggestion(err))
}

func TestResolveForeignQualifiedID(t *testing.T) {
	s := svc(t)
	// The fixture exists, but selecting another repository cannot reach it by ID.
	_, err := s.Resolve(context.Background(), "other", storetest.ID(model.KindFunction, "app.main"))
	assert.True(t, err != nil)
	_, err = s.CallGraph(context.Background(), "other", storetest.ID(model.KindFunction, "app.main"), "both", 2, 20)
	assert.True(t, err != nil)
}

func TestPackageContext(t *testing.T) {
	out, err := svc(t).SymbolContext(context.Background(), storetest.Repo, "example.com/app", ContextOptions{})
	require.NoError(t, err)
	require.NotNil(t, out.Package)
	assert.Equal(t, out.Package.Files, 1)
	names := []string{}
	for _, r := range out.Package.Hubs {
		names = append(names, r.QualifiedID)
	}
	assert.Contains(t, names, "app.Handler")
}

func TestKeywords(t *testing.T) {
	kw := keywords("Fix the ReplaceRepo retry in neo4jstore.(*Store).Get when the batch fails")
	assert.Contains(t, kw, "ReplaceRepo")
	assert.Contains(t, kw, "Replace")
	assert.Contains(t, kw, "Store")
	assert.NotContains(t, kw, "the")
	assert.LessOrEqual(t, len(kw), 12)

	// A typed question: the conversation around its words does not take
	// the cap from the words at its end (PLAN item 18, case l).
	kw = keywords(`what i can see i know my "github token key" can access private org-repos, but it's look like the indexing do not catch up and adding the repo's into my repo list? and thats mean when i wirte docs / code i lose the benefits of the system now if the repo is not public?`)
	for _, w := range []string{"see", "know", "but", "look", "like", "thats", "its", "lose", "benefits"} {
		assert.NotContains(t, kw, w)
	}
	assert.Contains(t, kw, "repo")
	assert.Contains(t, kw, "list")
	// Written as a name, a stopword still counts.
	assert.Contains(t, keywords("rename Like to Matches"), "Like")
}

func TestMinimalContextBudget(t *testing.T) {
	out, err := svc(t).MinimalContext(context.Background(), storetest.Repo, "Handler Do main Usage", nil, 120)
	require.NoError(t, err)
	assert.Equal(t, out.Budget, MinContextTokens)
	assert.LessOrEqual(t, out.UsedTokens, out.Budget)
}

func TestSymbolContextShowsDuplicates(t *testing.T) {
	ctx := context.Background()
	g := storetest.Fixture()
	handler := storetest.ID(model.KindFunction, "app.Handler")
	repoID := storetest.ID(model.KindRepository, storetest.Repo)
	rn, ok := g.Node(repoID)
	require.True(t, ok)
	enc, err := duplicates.Encode(duplicates.Report{Groups: []duplicates.Group{{
		Hash: "h", Kind: "exact", Similarity: 1, Scope: "repository", Copies: 2,
		Members: []duplicates.Member{{ID: handler, Name: "app.Handler"}, {ID: storetest.ID(model.KindFunction, "app.main"), Name: "app.main"}},
	}}})
	require.NoError(t, err)
	rn.Duplicates = enc
	s := memory.New()
	require.NoError(t, s.ReplaceRepo(ctx, storetest.Repo, g))

	got, err := Service{R: s}.SymbolContext(ctx, storetest.Repo, handler, ContextOptions{})
	require.NoError(t, err)
	require.NotNil(t, got.Duplicate)
	assert.Equal(t, got.Duplicate.Copies, 2)
	require.Len(t, got.Duplicate.Others, 1)
	assert.Equal(t, got.Duplicate.Others[0].Name, "app.main")

	got, err = Service{R: s}.SymbolContext(ctx, storetest.Repo, storetest.ID(model.KindMethod, "(*app.impl).Do"), ContextOptions{})
	require.NoError(t, err)
	assert.True(t, got.Duplicate == nil, "no copies, no field")
}

func TestPRContextNamesDuplicatedChanges(t *testing.T) {
	ctx := context.Background()
	g := storetest.Fixture()
	rn, ok := g.Node(storetest.ID(model.KindRepository, storetest.Repo))
	require.True(t, ok)
	do := storetest.ID(model.KindMethod, "(*app.impl).Do")
	enc, err := duplicates.Encode(duplicates.Report{Groups: []duplicates.Group{{
		Hash: "h", Kind: "near", Similarity: 0.8, Scope: "repository", Copies: 2,
		Members: []duplicates.Member{{ID: do, Name: "(*app.impl).Do"}, {ID: storetest.ID(model.KindFunction, "app.main"), Name: "app.main"}},
	}}})
	require.NoError(t, err)
	rn.Duplicates = enc
	s := memory.New()
	require.NoError(t, s.ReplaceRepo(ctx, storetest.Repo, g))

	got, err := Service{R: s}.PRContext(ctx, storetest.Repo, PROptions{Files: []string{"app/app.go"}})
	require.NoError(t, err)
	require.True(t, len(got.DuplicatedChanged) > 0)
	assert.Contains(t, got.DuplicatedChanged[0], "edited copies ~80% alike")
	assert.Contains(t, got.DuplicatedChanged[0], "app/app.go:")
}

func TestShortenModule(t *testing.T) {
	const mod = "github.com/acme/app"
	for _, tt := range []struct{ local, short string }{
		{"github.com/acme/app/store.Get", "~/store.Get"},
		{"(*github.com/acme/app/store.Store).Get", "(*~/store.Store).Get"},
		{"(github.com/acme/app/store.Store).Get", "(~/store.Store).Get"},
		{"github.com/acme/app.Main", "~.Main"},
		{"github.com/acme/app", "~"},
		{"github.com/acme/app-tools/x.F", "github.com/acme/app-tools/x.F"}, // another module that starts the same
		{"internal/store/store.go", "internal/store/store.go"},
		{"GET /x (github.com/acme/app/api)", "GET /x (~/api)"},                               // a route's handler package
		{"github.com/acme/app/x.Map[github.com/acme/app/y.K]", "~/x.Map[~/y.K]"},             // type arguments
		{"github.com/acme/app/x.Pair[int, github.com/acme/app/y.K]", "~/x.Pair[int, ~/y.K]"}, // after a comma
		{"github.com/acme/appx.F", "github.com/acme/appx.F"},                                 // not at a boundary
	} {
		t.Run(tt.local, func(t *testing.T) {
			assert.Equal(t, ShortenModule(tt.local, mod), tt.short)
			assert.Equal(t, ExpandModule(tt.short, mod), tt.local)
			assert.Equal(t, ShortenModule(tt.local, ""), tt.local, "no module recorded")
		})
	}
	assert.Equal(t, ExpandModule("docs/~notes.md", mod), "docs/~notes.md", "a tilde elsewhere stays")
	assert.Equal(t, ShortenModule("github.com/acme/app/x.F (~)", mod), "github.com/acme/app/x.F (~)",
		"a local with a \"~\" of its own is left whole: expanding could not tell the two apart")
}

type fakeRanker struct {
	hits []SemanticHit
	err  error
}

func (f fakeRanker) Rank(context.Context, string, int) ([]SemanticHit, error) { return f.hits, f.err }

func TestMinimalContextSemantic(t *testing.T) {
	handler := storetest.ID(model.KindFunction, "app.Handler")
	task := "who answers incoming calls" // shares no word with any symbol

	plain, err := svc(t).MinimalContext(context.Background(), storetest.Repo, task, nil, 0)
	require.NoError(t, err)
	for _, it := range plain.Items {
		assert.NotEqual(t, it.QualifiedID, "app.Handler", "no meaning, no lexical match")
	}

	ctx := WithSemantic(context.Background(), fakeRanker{hits: []SemanticHit{{ID: handler, Score: 0.9}}})
	got, err := svc(t).MinimalContext(ctx, storetest.Repo, task, nil, 0)
	require.NoError(t, err)
	require.True(t, len(got.Items) > 0)
	assert.Equal(t, got.Items[0].QualifiedID, "app.Handler", "found by meaning")

	ctx = WithSemantic(context.Background(), fakeRanker{err: errors.New("embedding model is down")})
	_, err = svc(t).MinimalContext(ctx, storetest.Repo, "Handler", nil, 0)
	require.NoError(t, err, "a failing ranker leaves the lexical answer")
}
