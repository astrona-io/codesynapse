package conventions

import (
	"fmt"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

const mod = "example.com/app"

// goRepo is a Go repository whose tests in three packages assert with an
// in-repository helper, use t.Run in half of them and have one fuzz test.
func goRepo() Input {
	g := model.NewGraph()
	in := Input{Graph: g, GoCalls: map[string][]string{}, Imports: map[string][]string{}, Module: mod}
	fn := func(pkg, name string, test bool) string {
		id := model.NodeID("r", model.KindFunction, mod+"/"+pkg+"."+name)
		g.AddNode(model.Node{ID: id, Kind: model.KindFunction, Repo: "r", Name: name, Package: mod + "/" + pkg, IsTest: test, Language: "go"})
		return id
	}
	equal := fn("internal/check", "Equal", false)
	logf := fn("internal/logx", "From", false)
	for _, pkg := range []string{"store", "api", "ingest"} {
		code := fn(pkg, "Run", false)
		g.AddEdge(model.Edge{From: code, To: logf, Kind: model.EdgeCalls})
		in.GoCalls["fmt.Errorf"] = append(in.GoCalls["fmt.Errorf"], code, code)
		in.GoCalls["errors.Is"] = append(in.GoCalls["errors.Is"], code)
		for i := range 4 {
			test := fn(pkg, fmt.Sprintf("TestRun%d", i), true)
			g.AddEdge(model.Edge{From: test, To: equal, Kind: model.EdgeCalls})
			if i%2 == 0 {
				in.GoCalls["(*testing.T).Run"] = append(in.GoCalls["(*testing.T).Run"], test)
			}
		}
	}
	fn("store", "FuzzDecode", true)
	in.GoCalls["errors.New"] = []string{model.NodeID("r", model.KindFunction, mod+"/store.Run")}
	in.GoCalls["(*log/slog.Logger).Info"] = []string{model.NodeID("r", model.KindFunction, mod+"/internal/logx.From")}
	in.Imports["github.com/spf13/cobra"] = []string{"r|Package|" + mod + "/cmd"}
	in.Imports["net/http"] = []string{"r|Package|" + mod + "/api", "r|Package|" + mod + "/cmd"}
	return in
}

func TestGoConventions(t *testing.T) {
	assert.Equal(t, Analyze(goRepo()), []string{
		"tests: assert with the repository's own helpers internal/check (12 calls), not a library like testify",
		"tests: table-driven with t.Run subtests in 6 of 12 test functions",
		"tests: 1 fuzz test (func FuzzX): parsers and decoders get one",
		"errors: fmt.Errorf (6 calls) and errors.New (1); errors.Is/As to compare (3)",
		"logging: log/slog (1 call), reached through internal/logx",
		"frameworks: HTTP: net/http (2 importers); CLI: cobra (1 importer)",
	})

	// A library instead: testify is named, and a helper called by fewer than
	// three tested packages is not a convention.
	in := goRepo()
	in.GoCalls["github.com/stretchr/testify/assert.Equal"] = []string{model.NodeID("r", model.KindFunction, mod+"/store.TestRun0")}
	got := Analyze(in)
	assert.Equal(t, got[0], "tests: assert with testify (1 call) and the repository's helpers internal/check (12 calls)")
}

func TestImportsAndFiles(t *testing.T) {
	g := model.NewGraph()
	file := func(path string, test bool) string {
		id := model.NodeID("r", model.KindFile, path)
		g.AddNode(model.Node{ID: id, Kind: model.KindFile, Repo: "r", File: path, IsTest: test})
		return id
	}
	in := Input{Graph: g, Imports: map[string][]string{
		"pytest":                {file("tests/test_api.py", true), file("tests/test_db.py", true)},
		"fastapi":               {file("app/main.py", false)},
		"sqlalchemy.orm":        {file("app/db.py", false)},
		"structlog":             {"r|File|app/main.py"},
		"Microsoft.AspNetCore":  {file("Api/Program.cs", false)},
		"pytest_asyncio.plugin": {"r|File|tests/test_api.py"},
	}}
	file("spec/models/user_spec.rb", true)
	file("spec/models/post_spec.rb", true)
	file("app/controllers/users_controller.rb", false)
	assert.Equal(t, Analyze(in), []string{
		"tests: RSpec (2 spec files), pytest (2 importers)", // equal counts: by name
		"logging: structlog (1 importer)",
		"frameworks: HTTP: ASP.NET Core (1 importer), FastAPI (1 importer), Rails (1 controller); database: SQLAlchemy (1 importer)",
	})
}

func TestGoPackage(t *testing.T) {
	for full, pkg := range map[string]string{
		"fmt.Errorf":                                "fmt",
		"(*log/slog.Logger).Info":                   "log/slog",
		"(net/http.Header).Get":                     "net/http",
		"github.com/stretchr/testify/require.Equal": "github.com/stretchr/testify/require",
	} {
		assert.Equal(t, goPackage(full), pkg, full)
	}
}

func TestNothingFound(t *testing.T) {
	assert.Equal(t, len(Analyze(Input{Graph: model.NewGraph()})), 0)
}
