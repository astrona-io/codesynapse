package neo4jstore

import (
	"context"
	"os"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/require"

	"github.com/astrona-io/codesynapse/internal/store/storetest"
)

// TestContract runs against a real Neo4j when CODESYNAPSE_TEST_NEO4J_URI is
// set, e.g. via `make test-integration` with docker compose running. It
// deletes the "fixture" repo, so never point it at a database you care about.
func TestContract(t *testing.T) {
	uri := os.Getenv("CODESYNAPSE_TEST_NEO4J_URI")
	if uri == "" {
		t.Skip("CODESYNAPSE_TEST_NEO4J_URI not set")
	}
	ctx := context.Background()
	s, err := New(ctx, Config{
		URI: uri, User: "neo4j", Password: os.Getenv("CODESYNAPSE_TEST_NEO4J_PASSWORD"), Database: "neo4j",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close(ctx) })
	require.NoError(t, s.DeleteRepo(ctx, storetest.Repo))
	storetest.Run(t, s)
}
