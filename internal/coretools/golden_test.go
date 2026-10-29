package coretools

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

var update = flag.Bool("update", false, "rewrite the golden answers in testdata")

// goldenStore indexes a small Go repository whose root module is
// example.com/app: a store package with a type, a method, a constructor
// that calls it, a test, and a route handled in the api package.
func goldenStore(t *testing.T) *memory.Store {
	t.Helper()
	const repo, mod = "acme/app", "example.com/app"
	id := func(kind model.NodeKind, local string) string { return model.NodeID(repo, kind, local) }
	g := model.NewGraph()
	add := func(n model.Node) { n.Repo = repo; g.AddNode(n) }
	add(model.Node{ID: id(model.KindRepository, repo), Kind: model.KindRepository, Name: repo, Meta: `{"go_module":"` + mod + `"}`})
	add(model.Node{ID: id(model.KindFile, "store/store.go"), Kind: model.KindFile, Name: "store.go", File: "store/store.go", Language: "go"})
	add(model.Node{ID: id(model.KindPackage, mod+"/store"), Kind: model.KindPackage, Name: "store", QualifiedName: mod + "/store"})
	add(model.Node{ID: id(model.KindType, mod+"/store.Store"), Kind: model.KindType, Name: "Store", QualifiedName: "store.Store",
		Signature: "type Store struct", File: "store/store.go", StartLine: 5, EndLine: 8})
	add(model.Node{ID: id(model.KindMethod, "(*"+mod+"/store.Store).Get"), Kind: model.KindMethod, Name: "Get", QualifiedName: "(*store.Store).Get",
		Signature: "func (s *Store) Get(key string) (string, error)", File: "store/store.go", StartLine: 10, EndLine: 20})
	add(model.Node{ID: id(model.KindFunction, mod+"/store.New"), Kind: model.KindFunction, Name: "New", QualifiedName: "store.New",
		Signature: "func New() *Store", File: "store/store.go", StartLine: 22, EndLine: 26})
	add(model.Node{ID: id(model.KindFunction, mod+"/store.TestGet"), Kind: model.KindFunction, Name: "TestGet", QualifiedName: "store.TestGet",
		Signature: "func TestGet(t *testing.T)", File: "store/store_test.go", StartLine: 3, EndLine: 9, IsTest: true})
	add(model.Node{ID: id(model.KindFunction, mod+"/api.handleGet"), Kind: model.KindFunction, Name: "handleGet", QualifiedName: "api.handleGet",
		Signature: "func handleGet(w http.ResponseWriter, r *http.Request)", File: "api/api.go", StartLine: 12, EndLine: 30})
	add(model.Node{ID: id(model.KindRoute, "GET /items ("+mod+"/api)"), Kind: model.KindRoute, Name: "/items", Signature: "http GET",
		QualifiedName: "GET /items (" + mod + "/api)", File: "api/api.go", StartLine: 8, EndLine: 8})
	edge := func(from, to string, kind model.EdgeKind) { g.AddEdge(model.Edge{From: from, To: to, Kind: kind}) }
	edge(id(model.KindFile, "store/store.go"), id(model.KindType, mod+"/store.Store"), model.EdgeDefines)
	edge(id(model.KindFile, "store/store.go"), id(model.KindMethod, "(*"+mod+"/store.Store).Get"), model.EdgeDefines)
	edge(id(model.KindFile, "store/store.go"), id(model.KindFunction, mod+"/store.New"), model.EdgeDefines)
	edge(id(model.KindType, mod+"/store.Store"), id(model.KindMethod, "(*"+mod+"/store.Store).Get"), model.EdgeHasMethod)
	edge(id(model.KindFunction, mod+"/store.New"), id(model.KindMethod, "(*"+mod+"/store.Store).Get"), model.EdgeCalls)
	edge(id(model.KindFunction, mod+"/store.TestGet"), id(model.KindMethod, "(*"+mod+"/store.Store).Get"), model.EdgeCalls)
	edge(id(model.KindFunction, mod+"/api.handleGet"), id(model.KindMethod, "(*"+mod+"/store.Store).Get"), model.EdgeCalls)
	edge(id(model.KindRoute, "GET /items ("+mod+"/api)"), id(model.KindFunction, mod+"/api.handleGet"), model.EdgeHandledBy)
	mem := memory.New()
	require.NoError(t, mem.ReplaceRepo(context.Background(), repo, g))
	return mem
}

// The core tools' answers with compact IDs, kept as golden files so a
// change to their shape shows in review. Rewrite them with
// go test ./internal/coretools -run TestGoldenAnswers -update.
func TestGoldenAnswers(t *testing.T) {
	ctx := context.Background()
	mem := goldenStore(t)
	const repo = "acme/app"
	for _, tt := range []struct {
		name string
		run  func() (any, error)
	}{
		{"file_outline", func() (any, error) { return FileOutline(ctx, mem, repo, FileIn{File: "store/store.go"}) }},
		{"search", func() (any, error) { return Search(ctx, mem, repo, SearchIn{Query: "Get"}) }},
		{"search_route", func() (any, error) { return Search(ctx, mem, repo, SearchIn{Query: "items", Kinds: []string{"Route"}}) }},
		{"symbol_context", func() (any, error) {
			return SymbolContext(ctx, mem, repo, SymbolContextIn{Symbol: "(*store.Store).Get", Verbosity: "full"})
		}},
		{"call_graph", func() (any, error) {
			return CallGraph(ctx, mem, repo, CallGraphIn{Symbol: "Method|(*~/store.Store).Get"})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			v, err := tt.run()
			require.NoError(t, err)
			got, err := json.MarshalIndent(v, "", "  ")
			require.NoError(t, err)
			got = append(got, '\n')
			assert.NotContains(t, string(got), "acme/app|", "no answer repeats the repository")
			assert.NotContains(t, string(got), "example.com/app/", "no answer repeats the module")
			path := filepath.Join("testdata", tt.name+".golden")
			if *update {
				require.NoError(t, os.MkdirAll("testdata", 0o755))
				require.NoError(t, os.WriteFile(path, got, 0o644))
			}
			want, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.True(t, bytes.Equal(got, want), "answer differs from "+path+":\n"+string(got))
		})
	}
}
