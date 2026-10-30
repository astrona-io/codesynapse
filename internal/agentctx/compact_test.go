package agentctx

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

const (
	compactRepo   = "acme/app"
	compactModule = "github.com/acme/app"
)

func TestCompactID(t *testing.T) {
	for _, tt := range []struct{ id, want string }{
		{"acme/app|Function|github.com/acme/app/x.F", "Function|~/x.F"},
		{"acme/app|Method|(*github.com/acme/app/x.T).M", "Method|(*~/x.T).M"},
		{"acme/app|Package|github.com/acme/app", "Package|~"},
		{"acme/app|Route|GET /x (github.com/acme/app/api)", "Route|GET /x (~/api)"},
		{"acme/app|Function|github.com/other/lib.F", "Function|github.com/other/lib.F"}, // outside the module
		{"acme/app|File|internal/x/x.go", "File|internal/x/x.go"},
		{"acme/app|Function|app.py:handler", "Function|app.py:handler"}, // other languages
		{"other/repo|Function|github.com/acme/app/x.F", "other/repo|Function|github.com/acme/app/x.F"},
		{"acme/app|NotAKind|x", "acme/app|NotAKind|x"},
		{"acme/app|plain text", "acme/app|plain text"},
	} {
		t.Run(tt.id, func(t *testing.T) {
			assert.Equal(t, CompactID(tt.id, compactRepo, compactModule), tt.want)
		})
	}
	assert.Equal(t, CompactID("acme/app|Function|github.com/acme/app/x.F", compactRepo, ""), "Function|github.com/acme/app/x.F",
		"no module recorded: only the repository goes")
}

func TestCompactAnswer(t *testing.T) {
	in := `{"id":"acme/app|Function|github.com/acme/app/x.F","package":"github.com/acme/app/x",` +
		`"callers":[{"id":"acme/app|Method|(*github.com/acme/app/y.T).M","line":12345678901}],` +
		`"other":"other/repo|Function|github.com/acme/app/x.F","summary":"see github.com/acme/app/x"}`
	got, err := CompactAnswer([]byte(in), compactRepo, compactModule)
	require.NoError(t, err)
	assert.Equal(t, string(got), `{"callers":[{"id":"Method|(*~/y.T).M","line":12345678901}],`+
		`"id":"Function|~/x.F","other":"other/repo|Function|github.com/acme/app/x.F",`+
		`"package":"~/x","summary":"see github.com/acme/app/x"}`)
	assert.NotContains(t, string(got), `"module"`, "the module is not stated per answer")

	_, err = CompactAnswer([]byte(`{`), compactRepo, compactModule)
	assert.Error(t, err)
}

// moduleService indexes a repository whose root module is compactModule,
// with two packages named store and one package outside the module.
func moduleService(t *testing.T) Service {
	t.Helper()
	g := model.NewGraph()
	add := func(kind model.NodeKind, local, qualified string) {
		g.AddNode(model.Node{ID: model.NodeID(compactRepo, kind, local), Kind: kind, Repo: compactRepo,
			Name: qualified[strings.LastIndex(qualified, ".")+1:], QualifiedName: qualified, File: "x.go", StartLine: 1, EndLine: 2})
	}
	g.AddNode(model.Node{ID: model.NodeID(compactRepo, model.KindRepository, compactRepo), Kind: model.KindRepository,
		Repo: compactRepo, Name: compactRepo, Meta: `{"go_module":"` + compactModule + `"}`})
	add(model.KindPackage, compactModule, compactModule)
	add(model.KindFunction, compactModule+"/internal/agentctx.CompactID", compactModule+"/internal/agentctx.CompactID")
	add(model.KindMethod, "(*"+compactModule+"/internal/store.Store).Get", "(*"+compactModule+"/internal/store.Store).Get")
	add(model.KindFunction, compactModule+"/internal/store.New", compactModule+"/internal/store.New")
	add(model.KindFunction, compactModule+"/pkg/store.New", compactModule+"/pkg/store.New")
	add(model.KindFunction, "internal/x.F", "internal/x.F") // a package path outside the module that looks relative
	add(model.KindFunction, compactModule+"/internal/x.F", compactModule+"/internal/x.F")
	s := memory.New()
	require.NoError(t, s.ReplaceRepo(context.Background(), compactRepo, g))
	return Service{R: s}
}

// Every compact form an answer writes, or an agent shortens further,
// resolves to the full node ID.
func TestResolveCompactForms(t *testing.T) {
	ctx := context.Background()
	s := moduleService(t)
	fn := model.NodeID(compactRepo, model.KindFunction, compactModule+"/internal/agentctx.CompactID")
	get := model.NodeID(compactRepo, model.KindMethod, "(*"+compactModule+"/internal/store.Store).Get")
	for _, tt := range []struct{ symbol, want string }{
		{fn, fn}, // full ID
		{"Function|" + compactModule + "/internal/agentctx.CompactID", fn},
		{"Function|~/internal/agentctx.CompactID", fn},
		{"Function|internal/agentctx.CompactID", fn}, // module left out
		{"~/internal/agentctx.CompactID", fn},        // no kind
		{"internal/agentctx.CompactID", fn},
		{compactModule + "/internal/agentctx.CompactID", fn},
		{"Method|(*~/internal/store.Store).Get", get},
		{"(*~/internal/store.Store).Get", get},
		{"(*internal/store.Store).Get", get},
		{"Package|~", model.NodeID(compactRepo, model.KindPackage, compactModule)},
		// The form as given wins: a package outside the module that
		// looks module-relative is not shadowed.
		{"internal/x.F", model.NodeID(compactRepo, model.KindFunction, "internal/x.F")},
		{"~/internal/x.F", model.NodeID(compactRepo, model.KindFunction, compactModule+"/internal/x.F")},
	} {
		t.Run(tt.symbol, func(t *testing.T) {
			n, err := s.Resolve(ctx, compactRepo, tt.symbol)
			require.NoError(t, err)
			assert.Equal(t, n.ID, tt.want)
			// The compact ID of the answer resolves back to the same node.
			back, err := s.Resolve(ctx, compactRepo, CompactID(n.ID, compactRepo, compactModule))
			require.NoError(t, err)
			assert.Equal(t, back.ID, n.ID)
		})
	}
}

// Colliding short names are suggested as compact IDs, and each of them
// resolves.
func TestAmbiguousCompactSuggestions(t *testing.T) {
	ctx := context.Background()
	s := moduleService(t)
	_, err := s.Resolve(ctx, compactRepo, "store.New")
	var sugg *SuggestionError
	require.True(t, errors.As(err, &sugg), "an ambiguous name suggests")
	assert.Equal(t, len(sugg.Suggestions), 2)
	for _, id := range sugg.Suggestions {
		assert.Contains(t, id, "Function|~/")
		assert.NotContains(t, id, compactRepo+"|")
		n, err := s.Resolve(ctx, compactRepo, id)
		require.NoError(t, err)
		assert.Equal(t, CompactID(n.ID, compactRepo, compactModule), id)
	}
}
