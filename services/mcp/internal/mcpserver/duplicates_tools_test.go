package mcpserver

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/parisnakitakejser/codesynapse/internal/duplicates"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/store/memory"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

// dupRepo is a repository whose functions A..., B... share one fingerprint
// per entry of hashes.
func dupRepo(t *testing.T, repo string, hashes ...string) *model.Graph {
	t.Helper()
	g := model.NewGraph()
	rid := model.NodeID(repo, model.KindRepository, repo)
	repoNode := g.AddNode(model.Node{ID: rid, Kind: model.KindRepository, Repo: repo, Name: repo})
	for i, h := range hashes {
		for _, name := range []string{"A", "B"} {
			if repo != "org/a" && name == "B" {
				continue // one copy per repository, the second repository has the other
			}
			g.AddNode(model.Node{ID: model.NodeID(repo, model.KindFunction, name+h), Kind: model.KindFunction, Repo: repo,
				Name: name + h, QualifiedName: "pkg" + string(rune('a'+i)) + "." + name + h, Package: "x/pkg" + string(rune('a'+i)),
				File: "f" + h + ".go", StartLine: 1, EndLine: 30, LOC: 30, CloneHash: h})
		}
	}
	enc, err := duplicates.Encode(duplicates.Analyze(g))
	require.NoError(t, err)
	repoNode.Duplicates = enc
	return g
}

func connectRepos(t *testing.T, graphs map[string]*model.Graph) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	s := memory.New()
	for repo, g := range graphs {
		require.NoError(t, s.ReplaceRepo(ctx, repo, g))
	}
	ct, st := mcp.NewInMemoryTransports()
	_, err := New(s, "test", Options{Reviews: s}).Connect(ctx, st, nil)
	require.NoError(t, err)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestDuplicateCode(t *testing.T) {
	// No stored report: a clear tool error.
	res := call(t, connect(t), "duplicate_code", nil, nil)
	assert.True(t, res.IsError)

	cs := connectRepos(t, map[string]*model.Graph{"org/a": dupRepo(t, "org/a", "h1", "h2")})
	var got duplicates.Report
	res = call(t, cs, "duplicate_code", map[string]any{"repo": "org/a"}, &got)
	require.False(t, res.IsError)
	require.Len(t, got.Groups, 2)
	assert.Equal(t, got.Summary.RedundantLOC, 60)
	assert.Equal(t, got.Groups[0].Scope, "file")
	assert.Len(t, got.Fingerprints, 0, "fingerprints are not sent")

	res = call(t, cs, "duplicate_code", map[string]any{"repo": "org/a", "scope": "repository"}, &got)
	require.False(t, res.IsError)
	assert.Len(t, got.Groups, 0, "both copies sit in one file")
}

func TestSharedCode(t *testing.T) {
	cs := connectRepos(t, map[string]*model.Graph{
		"org/a": dupRepo(t, "org/a", "h1", "h2", "h3"),
		"org/b": dupRepo(t, "org/b", "h1", "h2", "h3"),
	})
	var got duplicates.CrossReport
	res := call(t, cs, "shared_code", nil, &got)
	require.False(t, res.IsError)
	assert.Equal(t, got.Repos, 2)
	require.Len(t, got.Groups, 3)
	require.Len(t, got.Suggestions, 1, "three groups between the same two repositories")
	assert.Equal(t, got.Suggestions[0].Repos, []string{"org/a", "org/b"})

	res = call(t, cs, "shared_code", map[string]any{"repo_prefix": "org/a"}, &got)
	require.False(t, res.IsError)
	assert.Len(t, got.Groups, 0, "one repository has nothing to compare with")
}
