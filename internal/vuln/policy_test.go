package vuln

import (
	"os"
	"path/filepath"
	"testing"

	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestManifestPolicyIncludesAuxiliaryFiles(t *testing.T) {
	root := t.TempDir()
	for rel, data := range map[string]string{
		"package.json":              `{"dependencies":{"visible":"1"}}`,
		"yarn.lock":                 "visible@^1:\n  version \"1.0.0\"\n",
		"ignored/package-lock.json": `{"lockfileVersion":3,"packages":{"node_modules/hidden":{"version":"1.0.0"}}}`,
		"Cargo.lock":                "version = 3\n[[package]]\nname = \"visible_crate\"\nversion = \"1.0.0\"\nsource = \"registry+https://github.com/rust-lang/crates.io-index\"\n",
		"Cargo.toml":                "[dev-dependencies]\nvisible_crate = \"1\"\n",
		"ignored/go.mod":            "not a Go module\n",
		"go.mod":                    "module example.com/main\n\ngo 1.23\nrequire example.com/visible v1.2.3\n",
	} {
		p := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
		require.NoError(t, os.WriteFile(p, []byte(data), 0o600))
	}
	policy, err := repoconfig.Parse([]byte("version: 1\nignore:\n  paths: [ignored/**, package.json, Cargo.toml]\n"))
	require.NoError(t, err)
	mods, err := ManifestModules(root, policy)
	require.NoError(t, err)
	found := map[string]Module{}
	for _, m := range mods {
		found[m.Path] = m
	}
	_, hidden := found["hidden"]
	assert.False(t, hidden)
	// Excluded Cargo.toml cannot change a lockfile dependency to development-only.
	crate, ok := found["visible_crate"]
	require.True(t, ok)
	assert.False(t, crate.Dev)
	gomods, err := Modules(root, "go1.23.0", policy)
	require.NoError(t, err)
	gofound := false
	for _, m := range gomods {
		if m.Path == "example.com/visible" {
			gofound = true
		}
	}
	assert.True(t, gofound)
}
