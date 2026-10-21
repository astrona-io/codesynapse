package agentctx

import (
	"context"
	"testing"

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
