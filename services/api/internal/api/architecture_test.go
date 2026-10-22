package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/arch"
	"github.com/parisnakitakejser/codesynapse/internal/catalog"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/store/memory"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func recordsGraph(t *testing.T, repo string, files map[string]string) *model.Graph {
	t.Helper()
	root := t.TempDir()
	for rel, text := range files {
		p := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(text), 0o600))
	}
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID(repo, model.KindRepository, repo), Kind: model.KindRepository, Repo: repo, Name: repo, Head: "abc", Time: "2026-10-22T10:00:00Z"})
	require.NoError(t, arch.Extract(context.Background(), repo, root, g))
	return g
}

func TestArchitectureEndpoints(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, "org/a", recordsGraph(t, "org/a", map[string]string{
		"docs/adr/0001-a.md": "# A\n\n- Status: accepted\n- Topic: db\n- Components: api\n",
		"docs/adr/0002-b.md": "# B\n\n- Status: accepted\n- Topic: db\n- Components: api\n",
		"docs/adr/0003-c.md": "# C\n\n- Status: proposed\n",
	})))
	require.NoError(t, st.ReplaceRepo(ctx, "org/b", recordsGraph(t, "org/b", map[string]string{"docs/adr/0001-x.md": "# Hidden\n\n- Status: accepted\n"})))
	ts := httptest.NewServer(New(ctx, Config{SourcesRoot: t.TempDir(), Version: "test"}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)
	base := "/api/v1/repos/org%2Fa/architecture"

	var page catalog.Page
	require.Equal(t, get(t, ts, base, &page), 200)
	assert.Equal(t, page.Total, 3)
	assert.Equal(t, page.Source.Head, "abc")
	require.Equal(t, get(t, ts, base+"?binding=1&component=API", &page), 200)
	assert.Equal(t, page.Total, 2)
	require.Equal(t, get(t, ts, base+"?status=proposed", &page), 200)
	assert.Equal(t, page.Entries[0].ID, "ADR-0003")
	assert.False(t, page.Entries[0].Binding, "a proposal is not a rule")
	require.Equal(t, get(t, ts, base+"?limit=1&offset=1", &page), 200)
	assert.Equal(t, len(page.Entries), 1)
	assert.Equal(t, page.Total, 3)

	var d catalog.Detail
	require.Equal(t, get(t, ts, base+"/record?id=ADR-0002", &d), 200)
	assert.Equal(t, d.Title, "B")
	assert.Equal(t, get(t, ts, base+"/record?id=ADR-0099", nil), 404)
	assert.Equal(t, get(t, ts, base+"/record", nil), 400)
	assert.Equal(t, get(t, ts, base+"/record?id=ADR-0001", nil), 200)

	var issues struct {
		Issues []catalog.Issue `json:"issues"`
	}
	require.Equal(t, get(t, ts, base+"/issues", &issues), 200)
	require.Len(t, issues.Issues, 1)
	assert.Equal(t, issues.Issues[0].Kind, "potential-conflict")
	assert.Equal(t, issues.Issues[0].Records, []string{"ADR-0001", "ADR-0002"})

	var res catalog.ContextResult
	require.Equal(t, get(t, ts, base+"/context?components=api", &res), 200)
	assert.Equal(t, len(res.Binding), 2)
	assert.Equal(t, len(res.Issues), 1, "the possible conflict comes with the rules, unresolved")

	// Another repository's records are not in this catalog, and an unknown repository is a 404.
	require.Equal(t, get(t, ts, base+"?q=hidden", &page), 200)
	assert.Equal(t, page.Total, 0)
	assert.Equal(t, get(t, ts, "/api/v1/repos/nope/architecture", nil), 404)
}
