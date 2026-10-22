package neo4jstore

import (
	"context"
	"os"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestContractAtomicReplacement(t *testing.T) {
	uri := os.Getenv("CODESYNAPSE_TEST_NEO4J_URI")
	if uri == "" {
		t.Skip("CODESYNAPSE_TEST_NEO4J_URI not set")
	}
	ctx := context.Background()
	s, err := New(ctx, Config{URI: uri, User: "neo4j", Password: os.Getenv("CODESYNAPSE_TEST_NEO4J_PASSWORD"), Database: "neo4j"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close(ctx) })
	require.NoError(t, s.EnsureSchema(ctx))
	require.NoError(t, s.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	_, err = s.write(ctx, `CREATE CONSTRAINT cs_atomic_function_name IF NOT EXISTS FOR (n:Function) REQUIRE n.name IS UNIQUE`, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = s.write(ctx, `DROP CONSTRAINT cs_atomic_function_name IF EXISTS`, nil) })
	g := storetest.Fixture()
	for _, value := range g.Nodes() {
		n, _ := g.Node(value.ID)
		if n.Kind == model.KindFunction {
			n.Name = "conflicting-name"
		}
		if n.Kind == model.KindRepository {
			n.Head = "must-not-publish"
		}
	}
	require.Error(t, s.ReplaceRepo(ctx, storetest.Repo, g))
	node, err := s.Node(ctx, storetest.ID(model.KindRepository, storetest.Repo))
	require.NoError(t, err)
	assert.Equal(t, node.Head, "abc")
	node, err = s.Node(ctx, storetest.ID(model.KindFunction, "app.main"))
	require.NoError(t, err)
	assert.Equal(t, node.Name, "main")
}
