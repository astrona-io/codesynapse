package agentctx

import (
	"context"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/duplicates"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/store/memory"
	"github.com/parisnakitakejser/codesynapse/internal/store/storetest"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
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
}

func TestMinimalContextBudget(t *testing.T) {
	out, err := svc(t).MinimalContext(context.Background(), storetest.Repo, "Handler Do main Usage", nil, 120)
	require.NoError(t, err)
	assert.LessOrEqual(t, out.UsedTokens, 120)
	assert.Greater(t, out.Omitted, 0, "tight budget drops items")
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
		Members: []duplicates.Member{{ID: handler, Name: "app.Handler"}, {ID: "other|Function|x.Copy", Name: "x.Copy", File: "x/copy.go", StartLine: 3, EndLine: 20}},
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
	assert.Equal(t, got.Duplicate.Others[0].Name, "x.Copy")

	got, err = Service{R: s}.SymbolContext(ctx, storetest.Repo, storetest.ID(model.KindFunction, "app.main"), ContextOptions{})
	require.NoError(t, err)
	assert.True(t, got.Duplicate == nil, "no copies, no field")
}
