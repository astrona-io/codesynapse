package license

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
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
		"Metadata-Version: 2.1\nName: Requests\nVersion: 2.32.3\nLicense: Apache 2.0\nClassifier: License :: OSI Approved :: Apache Software License\n\nLicense: GPL in the body is ignored\n")
	writeFile(t, filepath.Join(root, ".venv", "lib", "python3.12", "site-packages", "new_pkg-1.0.dist-info", "METADATA"),
		"Metadata-Version: 2.4\nName: new_pkg\nVersion: 1.0.0\nLicense-Expression: MIT OR Apache-2.0\n")

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

func pyMetadata(t *testing.T, root, dir, name, version, licence string) {
	t.Helper()
	writeFile(t, filepath.Join(root, ".venv", "lib", "python3.12", "site-packages", dir, "METADATA"),
		"Metadata-Version: 2.4\nName: "+name+"\nVersion: "+version+"\nLicense-Expression: "+licence+"\n")
}

func TestPythonLicenceMustMatchTheRequestedVersion(t *testing.T) {
	root := t.TempDir()
	pyMetadata(t, root, "example-1.0.dist-info", "example", "1.0", "MIT")
	pyMetadata(t, root, "multi-1.0.dist-info", "multi", "1.0", "MIT")
	pyMetadata(t, root, "multi-2.0.dist-info", "multi", "2.0", "Apache-2.0")
	got := Resolver{Root: root, GoModCache: t.TempDir()}.Resolve(context.Background(), []Dep{
		{PyPI, "example", "2.0"},   // installed 1.0 is not evidence for 2.0
		{PyPI, "example", "1.0.0"}, // 1.0 and 1.0.0 are one version
		{PyPI, "example", "v1.0"},  // spelled differently
		{PyPI, "example", ""},      // one installed version: unambiguous
		{PyPI, "multi", "2.0"},     // the right one of two, not the first
		{PyPI, "multi", "1.0"},     //
		{PyPI, "multi", "3.0"},     // neither
		{PyPI, "multi", ""},        // two installed: ambiguous
		{PyPI, "absent", "1.0"},    // not installed
	})
	licence := func(d Dep) string { return got[d.Key()].License }
	assert.Equal(t, licence(Dep{PyPI, "example", "2.0"}), "")
	assert.Equal(t, licence(Dep{PyPI, "example", "1.0.0"}), "MIT")
	assert.Equal(t, licence(Dep{PyPI, "example", "v1.0"}), "MIT")
	assert.Equal(t, licence(Dep{PyPI, "example", ""}), "MIT")
	assert.Equal(t, licence(Dep{PyPI, "multi", "2.0"}), "Apache-2.0")
	assert.Equal(t, licence(Dep{PyPI, "multi", "1.0"}), "MIT")
	assert.Equal(t, licence(Dep{PyPI, "multi", "3.0"}), "")
	assert.Equal(t, licence(Dep{PyPI, "multi", ""}), "")
	assert.Equal(t, licence(Dep{PyPI, "absent", "1.0"}), "")
}

func TestPythonMetadataWithoutAVersionIsNotEvidence(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".venv", "lib", "python3.12", "site-packages", "odd-1.0.dist-info", "METADATA"),
		"Metadata-Version: 2.4\nName: odd\nLicense-Expression: MIT\n")
	got := Resolver{Root: root, GoModCache: t.TempDir()}.Resolve(context.Background(), []Dep{{PyPI, "odd", "1.0"}})
	assert.Equal(t, len(got), 0)
}

func TestCanonicalPyVersion(t *testing.T) {
	for _, same := range [][2]string{{"1.0", "1.0.0"}, {"1.0", "==1.0"}, {"V2.1", "2.1.0"}, {"1.0.post1", "1.0.0.post1"}, {"1.0rc1", "1.0.0rc1"}} {
		assert.True(t, samePyVersion(same[0], same[1]), same[0]+" == "+same[1])
	}
	for _, differ := range [][2]string{{"1.0", "1.1"}, {"1.0", "1.0.post1"}, {"1.0rc1", "1.0"}, {"10.0", "1.0"}} {
		assert.False(t, samePyVersion(differ[0], differ[1]), differ[0]+" != "+differ[1])
	}
}

// A METADATA symlink under an installed package can point outside the
// checkout, as can the dist-info folder itself. Neither is read.
func TestPythonMetadataOutsideTheRepositoryIsNotRead(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	marker := "License-Expression: LicenseRef-OutsideMarker"
	writeFile(t, filepath.Join(outside, "METADATA"), "Metadata-Version: 2.4\nName: fixture\nVersion: 1.0\n"+marker+"\n")
	site := filepath.Join(root, ".venv", "lib", "python3.12", "site-packages")
	require.NoError(t, os.MkdirAll(filepath.Join(site, "fixture-1.0.dist-info"), 0o750))
	require.NoError(t, os.Symlink(filepath.Join(outside, "METADATA"), filepath.Join(site, "fixture-1.0.dist-info", "METADATA")))
	writeFile(t, filepath.Join(outside, "other-2.0.dist-info", "METADATA"), "Metadata-Version: 2.4\nName: other\nVersion: 2.0\n"+marker+"\n")
	require.NoError(t, os.Symlink(filepath.Join(outside, "other-2.0.dist-info"), filepath.Join(site, "other-2.0.dist-info")))
	// An ordinary installed package in the same virtualenv still resolves,
	// including through a symlink that stays inside the repository.
	pyMetadata(t, root, "plain-3.0.dist-info", "plain", "3.0", "MIT")
	require.NoError(t, os.Symlink(filepath.Join(site, "plain-3.0.dist-info", "METADATA"), filepath.Join(root, "METADATA.link")))

	got := Resolver{Root: root, GoModCache: t.TempDir()}.Resolve(context.Background(), []Dep{
		{PyPI, "fixture", "1.0"}, {PyPI, "other", "2.0"}, {PyPI, "plain", "3.0"},
	})
	for k, f := range got {
		assert.NotContains(t, f.License, "OutsideMarker", k)
	}
	assert.Equal(t, got[Dep{PyPI, "plain", "3.0"}.Key()].License, "MIT")
	assert.Equal(t, len(got), 1)
}
