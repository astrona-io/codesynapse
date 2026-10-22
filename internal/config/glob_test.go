package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestGlobMatching(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"ci/*.yaml", "ci/build.yaml", true}, {"ci/*.yaml", "ci/nested/build.yaml", false},
		{"**/*_easyjson.go", "types_easyjson.go", true}, {"**/*_easyjson.go", "nested/deep/types_easyjson.go", true},
		{"vendor/**", "vendor", true}, {"vendor/**", "vendor/pkg/file.go", true}, {"vendor/**", "x/vendor/file.go", false},
		{"**/x/**/y", "x/y", true}, {"**/x/**/y", "a/x/b/c/y", true}, {"**/x/**/y", "a/x/b/c/z", false},
		{"file?.go", "fileé.go", true}, {"*.yaml", "App.YAML", false},
		{"a/**/**/b", "a/b", true},
	}
	for _, tt := range cases {
		t.Run(tt.pattern+"-"+tt.path, func(t *testing.T) {
			p, err := compilePattern(tt.pattern)
			require.NoError(t, err)
			assert.Equal(t, p.match(strings.Split(tt.path, "/")), tt.want)
		})
	}
	for _, s := range []string{"", "../x", "a/../b", "/tmp/x", "C:/x", `a\b`, "a//b", "./a", "a/./b", "a/", "!foo", "{a,b}", "[ab]", "a**b", "***", "foo/**bar", strings.Repeat("x", MaxPatternBytes+1), "x\x00"} {
		t.Run("invalid-"+s, func(t *testing.T) { _, err := compilePattern(s); assert.Error(t, err) })
	}
}
func TestPolicyMatchersAndWalk(t *testing.T) {
	p, err := Parse([]byte("version: 1\nignore:\n  paths: ['vendor/**', '**/dist/**', 'single.go']\nscanners:\n  kubernetes:\n    ignore_paths: ['ci/*.yaml']\n"))
	require.NoError(t, err)
	assert.True(t, p.PruneDir("vendor"))
	assert.True(t, p.PruneDir("app/dist"))
	assert.False(t, p.PruneDir("app"))
	assert.False(t, p.PathAllowed("vendor/pkg/file.go"))
	assert.False(t, p.PathAllowed("single.go"))
	assert.True(t, p.PathAllowed("ci/build.yaml"))
	assert.False(t, p.KubernetesAllowed("ci/build.yaml"))
	assert.True(t, p.KubernetesAllowed("deploy/service.yaml"))
	assert.False(t, p.KubernetesAllowed("vendor/service.yaml"))
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "vendor"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "single.go"), nil, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), nil, 0600))
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	for _, entry := range entries {
		allowed, err := p.WalkEntry(root, filepath.Join(root, entry.Name()), entry)
		if entry.IsDir() {
			assert.ErrorIs(t, err, fs.SkipDir)
			assert.False(t, allowed)
		} else {
			require.NoError(t, err)
			assert.Equal(t, allowed, entry.Name() == "main.go")
		}
	}
}
func FuzzGlob(f *testing.F) {
	f.Add("**/*.go", "dir/main.go")
	f.Add("**/**/**/x", "a/b/c/x")
	f.Add("../*", "secret")
	f.Fuzz(func(t *testing.T, glob, rel string) {
		p, err := compilePattern(glob)
		if err != nil || !validPath(rel) {
			return
		}
		got := p.match(strings.Split(rel, "/"))
		assert.Equal(t, p.match(strings.Split(rel, "/")), got)
	})
}
