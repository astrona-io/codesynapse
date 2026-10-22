package license

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

const mitText = "Permission is hereby granted, free of charge, to any person obtaining a copy"

func writeFile(t *testing.T, p, text string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
	require.NoError(t, os.WriteFile(p, []byte(text), 0o600))
}

func TestResolveLocal(t *testing.T) {
	root := t.TempDir()
	cache := t.TempDir()
	writeFile(t, filepath.Join(cache, "github.com", "!burnt!sushi", "toml@v1.4.0", "COPYING"), mitText)
	writeFile(t, filepath.Join(root, "vendor", "example.com", "lib", "LICENSE"), "GNU GENERAL PUBLIC LICENSE Version 3")
	writeFile(t, filepath.Join(root, "web", "node_modules", "left-pad", "package.json"), `{"version":"1.3.0","license":"WTFPL"}`)
	writeFile(t, filepath.Join(root, "web", "node_modules", "old", "package.json"), `{"version":"2.0.0","licenses":[{"type":"MIT"},{"type":"Apache 2.0"}]}`)
	writeFile(t, filepath.Join(root, "web", "node_modules", "other", "package.json"), `{"version":"9.9.9","license":"MIT"}`)
	writeFile(t, filepath.Join(root, ".venv", "lib", "python3.12", "site-packages", "Requests-2.32.3.dist-info", "METADATA"),
		"Metadata-Version: 2.1\nName: Requests\nLicense: Apache 2.0\nClassifier: License :: OSI Approved :: Apache Software License\n\nLicense: GPL in the body is ignored\n")
	writeFile(t, filepath.Join(root, ".venv", "lib", "python3.12", "site-packages", "new_pkg-1.0.dist-info", "METADATA"),
		"Metadata-Version: 2.4\nName: new_pkg\nLicense-Expression: MIT OR Apache-2.0\n")

	got := Resolver{Root: root, GoModCache: cache}.Resolve(context.Background(), []Dep{
		{Go, "stdlib", "v1.27.3"},
		{Go, "github.com/BurntSushi/toml", "v1.4.0"},
		{Go, "example.com/lib", "v0.1.0"},
		{Go, "example.com/missing", "v1.0.0"},
		{NPM, "left-pad", "1.3.0"},
		{NPM, "old", "2.0.0"},
		{NPM, "other", "1.0.0"}, // installed version differs: not trusted
		{PyPI, "requests", "2.32.3"},
		{PyPI, "new-pkg", "1.0"},
	})
	assert.Equal(t, got[Dep{Go, "stdlib", "v1.27.3"}.Key()], Found{"BSD-3-Clause", "Go standard library"})
	assert.Equal(t, got[Dep{Go, "github.com/BurntSushi/toml", "v1.4.0"}.Key()], Found{"MIT", "Go module cache: github.com/BurntSushi/toml@v1.4.0/COPYING"})
	assert.Equal(t, got[Dep{Go, "example.com/lib", "v0.1.0"}.Key()], Found{"GPL-3.0", "vendor/example.com/lib/LICENSE"})
	assert.Equal(t, got[Dep{NPM, "left-pad", "1.3.0"}.Key()], Found{"WTFPL", "web/node_modules/left-pad/package.json"})
	assert.Equal(t, got[Dep{NPM, "old", "2.0.0"}.Key()].License, "MIT OR Apache-2.0")
	assert.Equal(t, got[Dep{PyPI, "requests", "2.32.3"}.Key()].License, "Apache-2.0")
	assert.Equal(t, got[Dep{PyPI, "new-pkg", "1.0"}.Key()].License, "MIT OR Apache-2.0")
	for _, d := range []Dep{{Go, "example.com/missing", "v1.0.0"}, {NPM, "other", "1.0.0"}} {
		_, ok := got[d.Key()]
		assert.False(t, ok, d.Name)
	}
}

func TestResolveRejectsSymlinkOutOfRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "evil", "package.json"), `{"version":"1.0.0","license":"MIT"}`)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "node_modules"), 0o750))
	require.NoError(t, os.Symlink(filepath.Join(outside, "evil"), filepath.Join(root, "node_modules", "evil")))
	got := Resolver{Root: root, GoModCache: t.TempDir()}.Resolve(context.Background(), []Dep{{NPM, "evil", "1.0.0"}})
	assert.Equal(t, len(got), 0)
}

func TestDepsDev(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/v3/systems/NPM/packages/%40scope%2Fpkg/versions/1.0.0":
			_, _ = w.Write([]byte(`{"licenses":["MIT"]}`))
		case "/v3/systems/GO/packages/example.com%2Fa/versions/v1.0.0":
			_, _ = w.Write([]byte(`{"licenses":["MIT OR Apache-2.0","BSD-3-Clause"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := &DepsDev{BaseURL: srv.URL, HTTP: srv.Client()}
	root := t.TempDir()
	got := Resolver{Root: root, GoModCache: t.TempDir(), Online: c}.Resolve(context.Background(), []Dep{
		{NPM, "@scope/pkg", "1.0.0"}, {Go, "example.com/a", "v1.0.0"}, {PyPI, "gone", "1.0"},
	})
	assert.Equal(t, got[Dep{NPM, "@scope/pkg", "1.0.0"}.Key()], Found{"MIT", "deps.dev"})
	assert.Equal(t, got[Dep{Go, "example.com/a", "v1.0.0"}.Key()].License, "(MIT OR Apache-2.0) AND BSD-3-Clause")
	assert.Equal(t, Classify(got[Dep{Go, "example.com/a", "v1.0.0"}.Key()].License), Permissive)
	assert.Equal(t, len(got), 2)
}
