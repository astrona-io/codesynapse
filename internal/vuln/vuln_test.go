package vuln

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"

	"github.com/astrona-io/codesynapse/internal/model"
)

func TestModules(t *testing.T) {
	root := t.TempDir()
	write := func(p, s string) {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(root, p), []byte(s), 0o600))
	}
	write("go.mod", "module example.com/a\n\ngo 1.22\n\nrequire (\n\tgolang.org/x/net v0.17.0\n\tgithub.com/x/y v1.0.0 // indirect\n\texample.com/a/sub v0.0.0\n)\n")
	write("sub/go.mod", "module example.com/a/sub\n\ngo 1.23\ntoolchain go1.23.4\n\nrequire golang.org/x/net v0.20.0\n")
	write("nested/.git/HEAD", "ref: refs/heads/main\n")
	write("nested/go.mod", "module other\n\nrequire evil.example/x v1.0.0\n")

	mods, err := Modules(root, "go1.23.1")
	require.NoError(t, err)
	got := map[string]Module{}
	for _, m := range mods {
		got[m.Path] = m
	}
	assert.Equal(t, got["golang.org/x/net"].Version, "v0.20.0", "highest version wins")
	assert.True(t, got["github.com/x/y"].Indirect)
	assert.Equal(t, got[StdlibPath].Version, "v1.23.4", "newer toolchain line beats the build Go")

	old, err := Modules(root, "go1.27.3")
	require.NoError(t, err)
	for _, m := range old {
		if m.Path == StdlibPath {
			assert.Equal(t, m.Version, "v1.27.3", "go directive is a minimum, not the build version")
		}
	}
	_, own := got["example.com/a/sub"]
	assert.False(t, own, "in-repo modules excluded")
	_, nested := got["evil.example/x"]
	assert.False(t, nested, "nested repositories skipped")
}

func TestOSVSymbol(t *testing.T) {
	tests := []struct{ in, pkg, sym string }{
		{"net/http.Get", "net/http", "Get"},
		{"(*net/http.Client).Do", "net/http", "Client.Do"},
		{"(golang.org/x/net/html.Tokenizer).Next", "golang.org/x/net/html", "Tokenizer.Next"},
		{"(*example.com/p.List[T]).Push", "example.com/p", "List.Push"},
	}
	for _, tt := range tests {
		pkg, sym := osvSymbol(tt.in)
		assert.Equal(t, pkg, tt.pkg, tt.in)
		assert.Equal(t, sym, tt.sym, tt.in)
	}
}

func TestScanAndApply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/querybatch":
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{
				map[string]any{"vulns": []any{map[string]string{"id": "GO-2024-0001"}, map[string]string{"id": "GHSA-aaaa"}}},
				map[string]any{},
			}})
		case strings.HasSuffix(r.URL.Path, "/GO-2024-0001"):
			_, _ = w.Write([]byte(`{"id":"GO-2024-0001","summary":"HTTP/2 flood","aliases":["CVE-2024-1","GHSA-aaaa"],
				"affected":[{"package":{"name":"golang.org/x/net","ecosystem":"Go"},
				"ranges":[{"type":"SEMVER","events":[{"introduced":"0"},{"fixed":"0.23.0"},{"introduced":"0.30.0"},{"fixed":"0.31.0"}]}],
				"ecosystem_specific":{"imports":[{"path":"golang.org/x/net/http2","symbols":["Server.ServeConn","Transport.RoundTrip"]}]}}],
				"database_specific":{"url":"https://pkg.go.dev/vuln/GO-2024-0001"}}`))
		case strings.HasSuffix(r.URL.Path, "/GHSA-aaaa"):
			_, _ = w.Write([]byte(`{"id":"GHSA-aaaa","aliases":["CVE-2024-1","GO-2024-0001"],
				"affected":[{"package":{"name":"golang.org/x/net","ecosystem":"Go"}}],
				"database_specific":{"severity":"HIGH","cwe_ids":["CWE-400"]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	mods := []Module{{Path: "golang.org/x/net", Version: "v0.17.0"}, {Path: StdlibPath, Version: "v1.23.0"}}
	vulns, err := c.Scan(context.Background(), mods)
	require.NoError(t, err)
	require.Len(t, vulns, 1, "GHSA duplicate folded into GO- entry")
	v := vulns[0]
	assert.Equal(t, v.Severity, "high")
	assert.Equal(t, v.CWE, []string{"CWE-400"})
	assert.Equal(t, v.Fixed, "v0.23.0")

	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID("r", model.KindRepository, "r"), Kind: model.KindRepository, Repo: "r", Name: "r"})
	caller := model.NodeID("r", model.KindFunction, "app.serve")
	g.AddNode(model.Node{ID: caller, Kind: model.KindFunction, Repo: "r", Name: "serve"})
	Apply("r", g, mods, vulns,
		map[string][]string{"(*golang.org/x/net/http2.Server).ServeConn": {caller}},
		map[string][]string{"golang.org/x/net/http2": {"r|Package|app"}}, nil)

	vn, ok := g.Node(model.NodeID("r", model.KindVulnerability, "golang.org/x/net@GO-2024-0001"))
	require.True(t, ok)
	assert.Equal(t, vn.Reachability, Called)
	assert.Contains(t, vn.Aliases, "CVE-2024-1")
	ext := model.NodeID("r", model.KindExternal, "golang.org/x/net/http2.Server.ServeConn")
	found := false
	for _, e := range g.Edges() {
		if e.From == caller && e.To == ext && e.Kind == model.EdgeCalls {
			found = true
		}
	}
	assert.True(t, found, "caller linked to vulnerable symbol")
}

func TestSemverGo(t *testing.T) {
	for in, want := range map[string]string{"go1.27.3": "v1.27.3", "1.22": "v1.22.0", "go1.27.3 X:boringcrypto": "v1.27.3", "devel go1.28-abc": "", "": ""} {
		assert.Equal(t, semverGo(in), want, in)
	}
}

func TestManifestModules(t *testing.T) {
	root := t.TempDir()
	write := func(p, s string) {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(root, p), []byte(s), 0o600))
	}
	write("web/package.json", `{"dependencies":{"lodash":"^4.17.0"},"devDependencies":{"vitest":"^1"}}`)
	write("web/package-lock.json", `{"lockfileVersion":3,"packages":{"":{},"node_modules/lodash":{"version":"4.17.20","license":"MIT"},"node_modules/vitest":{"version":"1.0.0","dev":true,"license":{"type":"Apache 2.0"}},"node_modules/lodash/node_modules/x":{"version":"1.0.0"}}}`)
	write("svc/requirements.txt", "Flask==2.0.1  # web\nrequests>=2\nPyYAML==5.3\n")
	write("api/pyproject.toml", "[project]\ndependencies = [\"fastapi>=0.1\"]\n")
	write("api/uv.lock", "[[package]]\nname = \"fastapi\"\nversion = \"0.100.0\"\n\n[[package]]\nname = \"starlette\"\nversion = \"0.27.0\"\n")
	write("web/node_modules/evil/package-lock.json", `{"packages":{"node_modules/evil":{"version":"1"}}}`)

	mods, err := ManifestModules(root)
	require.NoError(t, err)
	got := map[string]Module{}
	for _, m := range mods {
		got[m.Key()] = m
	}
	assert.Equal(t, got["npm:lodash"].Version, "4.17.20")
	assert.False(t, got["npm:lodash"].Indirect)
	assert.True(t, got["npm:x"].Indirect, "nested node_modules are transitive")
	assert.Equal(t, got["npm:lodash"].License, "MIT")
	assert.Equal(t, got["npm:lodash"].LicenseSource, "package-lock.json")
	assert.False(t, got["npm:lodash"].Dev)
	assert.True(t, got["npm:vitest"].Dev)
	assert.Equal(t, got["npm:vitest"].License, "Apache-2.0", "deprecated {type} form, normalised")
	assert.Equal(t, got["PyPI:flask"].Version, "2.0.1", "PEP 503 normalised")
	_, ranged := got["PyPI:requests"]
	assert.False(t, ranged, "unpinned requirements cannot be checked")
	assert.Equal(t, ImportName(got["PyPI:pyyaml"]), "yaml")
	assert.False(t, got["PyPI:fastapi"].Indirect)
	assert.True(t, got["PyPI:starlette"].Indirect)
	_, evil := got["npm:evil"]
	assert.False(t, evil, "node_modules is never scanned")
}

func TestApplyImportedByName(t *testing.T) {
	for _, tt := range []struct {
		eco, module, imported string
		want                  string
	}{
		{EcosystemNuGet, "Newtonsoft.Json", "Newtonsoft.Json.Linq", Imported},
		{EcosystemNuGet, "Newtonsoft.Json", "Newtonsoft.Json", Imported},
		{EcosystemNuGet, "Newtonsoft.Json", "Newtonsoft.JsonX", Required},
		{EcosystemCargo, "serde-json", "serde_json", Imported},
		{EcosystemRubyGems, "rack", "rack/utils", Imported},
		{EcosystemRubyGems, "net-http", "net/http", Imported},
		{EcosystemRubyGems, "rack", "rackup", Required},
		{EcosystemNPM, "lodash", "lodash/fp", Imported},
		{EcosystemNPM, "lodash", "lodash.merge", Required},
	} {
		t.Run(tt.eco+" "+tt.imported, func(t *testing.T) {
			g := model.NewGraph()
			v := Vulnerability{ID: "GHSA-x", Module: tt.module, Ecosystem: tt.eco, Version: "1.0.0"}
			Apply("r", g, []Module{{Path: tt.module, Version: "1.0.0", Ecosystem: tt.eco}}, []Vulnerability{v}, nil,
				map[string][]string{tt.imported: {"r|File|a"}}, nil)
			n, ok := g.Node(model.NodeID("r", model.KindVulnerability, Module{Path: tt.module, Ecosystem: tt.eco}.Key()+"@GHSA-x"))
			require.True(t, ok)
			assert.Equal(t, n.Reachability, tt.want)
		})
	}
}

// A replace directive changes what is built: the version (or the module) the
// code actually uses is what inventory, advisories and licences describe.
func TestGoReplacementsDecideTheBuiltVersion(t *testing.T) {
	root := t.TempDir()
	write := func(p, s string) {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(root, p), []byte(s), 0o600))
	}
	write("go.mod", `module example.com/app

go 1.22

require (
	example.com/library v1.2.0
	example.com/pinned v1.5.0
	example.com/forked v1.1.0
	example.com/only v1.2.0
	example.com/local v1.0.0
	example.com/plain v1.0.0
)

replace example.com/library => example.com/library v1.0.0

replace example.com/pinned v1.5.0 => example.com/pinned v1.4.0

replace example.com/only v1.9.9 => example.com/only v1.0.0

replace example.com/forked => github.com/fork/forked v1.1.1

replace example.com/local => ../local
`)
	// A nested module's replaces apply to its own requirements, not the root's.
	write("tools/go.mod", "module example.com/app/tools\n\ngo 1.22\n\nrequire example.com/plain v1.0.0\n\nreplace example.com/plain => example.com/plain v0.9.0\n")

	mods, err := Modules(root, "go1.23.1")
	require.NoError(t, err)
	got := map[string]Module{}
	for _, m := range mods {
		got[m.Path] = m
	}
	assert.Equal(t, got["example.com/library"].Version, "v1.0.0", "same module at the replacement version")
	assert.True(t, got["example.com/library"].Replacement == nil, "nothing to report beyond the version")
	assert.Equal(t, got["example.com/pinned"].Version, "v1.4.0", "a version-specific replace")
	assert.Equal(t, got["example.com/only"].Version, "v1.2.0", "a replace for another version does not apply")

	fork := got["example.com/forked"]
	assert.Equal(t, fork.Version, "v1.1.1")
	require.True(t, fork.Replacement != nil)
	assert.Equal(t, fork.Replacement.Path, "github.com/fork/forked")
	assert.Equal(t, fork.Path, "example.com/forked", "the import path stays the identity")
	assert.Equal(t, fork.BuildPath(), "github.com/fork/forked")
	assert.False(t, fork.Local())

	local := got["example.com/local"]
	require.True(t, local.Replacement != nil)
	assert.True(t, local.Local())
	assert.Equal(t, local.Version, "", "a local directory has no published version")
	assert.Equal(t, local.Replacement.Path, "../local")

	assert.Equal(t, got["example.com/plain"].Version, "v1.0.0", "the root requirement wins; the nested module's replace is its own")
}

func TestScanAsksAboutTheBuiltModule(t *testing.T) {
	var asked []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Queries []struct {
				Package struct{ Name string } `json:"package"`
				Version string                `json:"version"`
			} `json:"queries"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		results := make([]any, len(body.Queries))
		for i, q := range body.Queries {
			asked = append(asked, map[string]string{"name": q.Package.Name, "version": q.Version})
			results[i] = map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	_, err := c.Scan(context.Background(), []Module{
		{Path: "example.com/library", Version: "v1.0.0"},
		{Path: "example.com/forked", Version: "v1.1.1", Replacement: &Replacement{Path: "github.com/fork/forked"}},
		{Path: "example.com/local", Replacement: &Replacement{Path: "../local", Local: true}},
	})
	require.NoError(t, err)
	assert.Equal(t, len(asked), 2, "the local replacement is not looked up")
	assert.Equal(t, asked[0]["name"]+"@"+asked[0]["version"], "example.com/library@v1.0.0")
	assert.Equal(t, asked[1]["name"]+"@"+asked[1]["version"], "github.com/fork/forked@v1.1.1", "advisories of the fork, not of the original")

	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID("r", model.KindRepository, "r"), Kind: model.KindRepository, Repo: "r", Name: "r"})
	Apply("r", g, []Module{
		{Path: "example.com/forked", Version: "v1.1.1", Replacement: &Replacement{Path: "github.com/fork/forked"}},
		{Path: "example.com/local", Replacement: &Replacement{Path: "../local", Local: true}},
	}, nil, nil, nil, nil)
	forked, ok := g.Node(model.NodeID("r", model.KindModule, "example.com/forked"))
	require.True(t, ok)
	assert.Contains(t, forked.Summary, "github.com/fork/forked")
	localNode, _ := g.Node(model.NodeID("r", model.KindModule, "example.com/local"))
	assert.Contains(t, localNode.Summary, "local directory ../local")
}

// An advisory that hits one of several installed versions is attached to that
// version's module, and the two versions' advisories do not share a node.
func TestApplyKeepsAdvisoriesOfEachInstalledVersion(t *testing.T) {
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID("r", model.KindRepository, "r"), Kind: model.KindRepository, Repo: "r", Name: "r"})
	mods := []Module{
		{Path: "shared", Version: "1.0.0", Ecosystem: EcosystemNPM},
		{Path: "shared", Version: "2.0.0", Ecosystem: EcosystemNPM, Indirect: true, Requires: nil},
		{Path: "top", Version: "1.0.0", Ecosystem: EcosystemNPM, Requires: []string{"npm:shared"}, Resolved: []string{"npm:shared@2.0.0"}},
	}
	vulns := []Vulnerability{
		{ID: "GHSA-x", Module: "shared", Ecosystem: EcosystemNPM, Version: "1.0.0", Severity: "high"},
		{ID: "GHSA-x", Module: "shared", Ecosystem: EcosystemNPM, Version: "2.0.0", Severity: "high"},
	}
	Apply("r", g, mods, vulns, nil, nil, nil)

	plain := model.NodeID("r", model.KindModule, "npm:shared")
	second := model.NodeID("r", model.KindModule, "npm:shared@2.0.0")
	one, ok1 := g.Node(plain)
	two, ok2 := g.Node(second)
	require.True(t, ok1 && ok2)
	assert.Equal(t, one.Version, "1.0.0")
	assert.Equal(t, two.Version, "2.0.0")

	affects := map[string]string{} // advisory version -> module version it points at
	for _, e := range g.Edges() {
		if e.Kind != model.EdgeAffects {
			continue
		}
		v, _ := g.Node(e.From)
		m, _ := g.Node(e.To)
		affects[v.Version] = m.Version
	}
	assert.Equal(t, affects["1.0.0"], "1.0.0")
	assert.Equal(t, affects["2.0.0"], "2.0.0")

	var requires []string
	top := model.NodeID("r", model.KindModule, "npm:top")
	for _, e := range g.Edges() {
		if e.Kind == model.EdgeDependsOn && e.From == top {
			requires = append(requires, e.To)
		}
	}
	assert.Equal(t, strings.Join(requires, ","), second, "top uses the copy the lockfile resolved")
}

func TestApplyMarksCodeAbsentFromTheBuild(t *testing.T) {
	const mod, pkg = "example.com/lib", "example.com/lib/p"
	for _, tt := range []struct {
		name    string
		symbols map[string][]string
		built   map[string]bool
		absent  bool
	}{
		{"package not compiled in", map[string][]string{pkg: {"Parse"}}, map[string]bool{"example.com/lib/other": true}, true},
		{"compiled in through a dependency", map[string][]string{pkg: {"Parse"}}, map[string]bool{pkg: true}, false},
		{"build not loaded", map[string][]string{pkg: {"Parse"}}, nil, false},
		{"advisory names no packages", nil, map[string]bool{}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := model.NewGraph()
			v := Vulnerability{ID: "GO-1", Module: mod, Ecosystem: EcosystemGo, Version: "v1.0.0", Symbols: tt.symbols}
			Apply("r", g, []Module{{Path: mod, Version: "v1.0.0"}}, []Vulnerability{v}, nil, nil, tt.built)
			n, ok := g.Node(model.NodeID("r", model.KindVulnerability, mod+"@GO-1"))
			require.True(t, ok)
			assert.Equal(t, n.CodeAbsent, tt.absent)
		})
	}
}
