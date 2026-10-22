package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/arch"
	"github.com/parisnakitakejser/codesynapse/internal/catalog"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/store"
	"github.com/parisnakitakejser/codesynapse/internal/store/memory"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

// archGraph indexes records on top of a one-package code graph.
func archGraph(t *testing.T, repo string, records map[string]string) *model.Graph {
	t.Helper()
	root := t.TempDir()
	for rel, text := range records {
		p := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(text), 0o600))
	}
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID(repo, model.KindRepository, repo), Kind: model.KindRepository, Repo: repo, Name: repo, Head: "abc", Time: "2026-10-22T10:00:00Z"})
	g.AddNode(model.Node{ID: model.NodeID(repo, model.KindPackage, "ex.com/app/store"), Kind: model.KindPackage, Repo: repo, Name: "store", QualifiedName: "ex.com/app/store"})
	g.AddNode(model.Node{ID: model.NodeID(repo, model.KindFunction, "ex.com/app/store.Open"), Kind: model.KindFunction, Repo: repo, Name: "Open",
		QualifiedName: "ex.com/app/store.Open", Package: "ex.com/app/store", File: "store/store.go", StartLine: 1, EndLine: 9, Exported: true})
	require.NoError(t, arch.Extract(context.Background(), repo, root, g))
	return g
}

var archRecords = map[string]string{
	"docs/adr/0001-old.md":  "# Old\n\n- Status: accepted\n- Topic: storage\n",
	"docs/adr/0002-new.md":  "---\nstatus: accepted\ntopic: storage\ncomponents: [store]\nsupersedes: ADR-0001\nimplements: [store.Open]\n---\n# New\n\n## Decision\n\nUse the new store everywhere.\n",
	"docs/adr/0003-idea.md": "# Idea\n\n- Status: proposed\n- Components: store\n",
}

func TestArchitectureTools(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	cs := connectRepos(t, map[string]*model.Graph{
		"org/a": archGraph(t, "org/a", archRecords),
		"org/b": archGraph(t, "org/b", map[string]string{"docs/adr/0001-secret.md": "# Secret\n\n- Status: accepted\n"}),
	})

	var page catalog.Page
	res := call(t, cs, "architecture_search", map[string]any{"repo": "org/a", "binding_only": true}, &page)
	require.False(t, res.IsError)
	require.Len(t, page.Entries, 1, "ADR-0001 is superseded and ADR-0003 is only proposed")
	assert.Equal(t, page.Entries[0].ID, "ADR-0002")
	assert.True(t, page.Entries[0].Binding)
	assert.Equal(t, page.Source.Head, "abc", "the answer says which revision it describes")

	// The tool and the web API are the same service: same filter, same answer.
	require.NoError(t, s.ReplaceRepo(ctx, "org/a", archGraph(t, "org/a", archRecords)))
	want, err := catalog.Service{R: store.ForRepository(s, "org/a")}.List(ctx, "org/a", catalog.Filter{BindingOnly: true, Limit: 20})
	require.NoError(t, err)
	a, _ := json.Marshal(want)
	b, _ := json.Marshal(page)
	assert.Equal(t, string(a), string(b))

	var d catalog.Detail
	res = call(t, cs, "architecture_get", map[string]any{"repo": "org/a", "id": "ADR-0001"}, &d)
	require.False(t, res.IsError)
	assert.False(t, d.Binding)
	require.Len(t, d.Chain, 1)
	assert.Equal(t, d.Chain[0].ID, "ADR-0002")
	assert.Equal(t, len(d.Problems), 1, "still accepted although replaced")

	var ctxRes catalog.ContextResult
	res = call(t, cs, "architecture_context", map[string]any{"repo": "org/a", "symbols": []string{"store.Open"}, "task": "open the store"}, &ctxRes)
	require.False(t, res.IsError)
	require.Len(t, ctxRes.Binding, 1)
	assert.Equal(t, ctxRes.Binding[0].ID, "ADR-0002")

	var issues architectureIssuesOut
	res = call(t, cs, "architecture_issues", map[string]any{"repo": "org/a"}, &issues)
	require.False(t, res.IsError)
	kinds := map[string]bool{}
	for _, is := range issues.Issues {
		kinds[is.Kind] = true
	}
	assert.True(t, kinds["supersession-mismatch"])

	// Errors are tool errors, not crashes; another repository is not reachable by ID.
	assert.True(t, call(t, cs, "architecture_get", map[string]any{"repo": "org/a", "id": "ADR-9999"}, nil).IsError)
	assert.True(t, call(t, cs, "architecture_get", map[string]any{"repo": "org/a"}, nil).IsError, "an id is required")
	res = call(t, cs, "architecture_search", map[string]any{"repo": "org/a", "query": "secret"}, &page)
	require.False(t, res.IsError)
	assert.Equal(t, page.Total, 0, "org/b's records do not appear in org/a's catalog")
}
