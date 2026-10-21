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

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"

	"github.com/parisnakitakejser/codesynapse/internal/model"
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
		map[string][]string{"golang.org/x/net/http2": {"r|Package|app"}})

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
	write("web/package-lock.json", `{"lockfileVersion":3,"packages":{"":{},"node_modules/lodash":{"version":"4.17.20"},"node_modules/vitest":{"version":"1.0.0","dev":true},"node_modules/lodash/node_modules/x":{"version":"1.0.0"}}}`)
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
	assert.Equal(t, got["PyPI:flask"].Version, "2.0.1", "PEP 503 normalised")
	_, ranged := got["PyPI:requests"]
	assert.False(t, ranged, "unpinned requirements cannot be checked")
	assert.Equal(t, ImportName(got["PyPI:pyyaml"]), "yaml")
	assert.False(t, got["PyPI:fastapi"].Indirect)
	assert.True(t, got["PyPI:starlette"].Indirect)
	_, evil := got["npm:evil"]
	assert.False(t, evil, "node_modules is never scanned")
}
