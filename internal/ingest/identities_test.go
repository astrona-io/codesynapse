package ingest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/astrona-io/codesynapse/internal/identity"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestStoredIngestKeepsIdentity(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st := memory.New()
	file := filepath.Join(root, "app.js")
	require.NoError(t, os.WriteFile(file, []byte("export function compute(x) {\n return x + 1;\n}\n"), 0o600))
	g, err := Run(ctx, Options{Repo: "r", Root: root})
	require.NoError(t, err)
	require.NoError(t, st.ReplaceRepo(ctx, "r", g))
	previous, err := IdentityBaseline(ctx, st, "r")
	require.NoError(t, err)
	r, err := identity.Decode(previous, "r")
	require.NoError(t, err)
	ids := map[string]string{}
	for _, s := range r.Symbols {
		ids[s.Current.NodeID] = s.ID
	}
	require.NoError(t, os.WriteFile(file, []byte("// ordinary edit\nexport function compute(x) {\n return x + 2;\n}\n"), 0o600))
	next, err := Run(ctx, Options{Repo: "r", Root: root, PreviousIdentities: previous})
	require.NoError(t, err)
	count := 0
	for _, n := range next.Nodes() {
		if n.Kind == model.KindFunction {
			count++
			assert.Equal(t, n.SymbolID, ids[n.ID])
		}
	}
	assert.Equal(t, count, 1)
	_, err = Run(ctx, Options{Repo: "r", Root: root, PreviousIdentities: "corrupt"})
	assert.True(t, err != nil)
	unchanged, err := IdentityBaseline(ctx, st, "r")
	require.NoError(t, err)
	assert.Equal(t, unchanged, previous)
}
