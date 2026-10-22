package neo4jstore

import (
	"context"
	"os"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"

	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/store/storetest"
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

func TestPropsRoundTrip(t *testing.T) {
	n := model.Node{
		ID: "r|Function|f", Kind: model.KindFunction, Repo: "r", Name: "f", QualifiedName: "p.f",
		File: "a.go", StartLine: 3, EndLine: 9, Complexity: 4, Exported: true, SecurityTags: []string{"exec", "sql"},
	}
	p := toProps(n)
	_, hasSummary := p["summary"]
	assert.False(t, hasSummary, "zero values are omitted")

	// Neo4j returns lists as []any.
	p["security_tags"] = []any{"exec", "sql"}
	assert.Equal(t, fromProps(p), n)

	mod := model.Node{ID: "r|Module|npm:x", Kind: model.KindModule, Repo: "r", Name: "x", Version: "1.0.0", Ecosystem: "npm",
		License: "MIT OR Apache-2.0", LicenseSource: "package-lock.json", Dev: true}
	assert.Equal(t, fromProps(toProps(mod)), mod)
	repoNode := model.Node{ID: "r|Repository|r", Kind: model.KindRepository, Repo: "r", Name: "r", License: "Apache-2.0", LicenseSource: "LICENSE", Licenses: `{"total":1}`, Coverage: `{"yaml":3}`}
	assert.Equal(t, fromProps(toProps(repoNode)), repoNode)
	assert.Equal(t, fromProps(toProps(mod)), mod)
}
