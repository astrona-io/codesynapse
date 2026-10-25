package security

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestInventory(t *testing.T) {
	root := t.TempDir()
	write := func(rel string) {
		p := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte("x"), 0o600))
	}
	write("Dockerfile")
	write("svc/Dockerfile.dev")
	write("infra/main.tf")
	write("scripts/run.SH")
	write("node_modules/pkg/Dockerfile")
	write("testdata/x.tf")
	write("src/Main.java")

	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "evil.tf"), []byte("x"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "link")))

	got := Inventory(root)
	assert.Equal(t, got["Terraform"], 1)
	assert.Equal(t, got["Shell"], 1)
	assert.Equal(t, len(got), 2, "Dockerfiles and Java have rules")
}

func TestCoverageLines(t *testing.T) {
	c := Coverage{
		Code: map[string]int{"Go": 3}, YAML: 2, YAMLUnparseable: 1,
		HelmScanned: 1, HelmTotal: 2, NotCovered: map[string]int{"Terraform": 4},
	}
	checked, not := c.Lines()
	assert.Equal(t, checked[0], "3 Go file(s)")
	assert.Equal(t, len(not), 3)
	assert.True(t, c.Scannable(), "scannable")
	assert.False(t, Coverage{}.Scannable(), "empty coverage")
}
