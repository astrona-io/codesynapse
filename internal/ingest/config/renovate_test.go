package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

const workflow = "on: push\njobs:\n  b:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n"

func TestDependencyUpdateRules(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  []string // "rule@file:line"
	}{
		{
			name:  "manifests without automation",
			files: map[string]string{"go.mod": "module x\n", "web/package.json": "{}", "Dockerfile": "FROM scratch\n"},
			want:  []string{"dependency-updates-missing@go.mod:1"},
		},
		{
			name:  "nested manifest is the anchor without a root one",
			files: map[string]string{"svc/b/requirements-dev.txt": "x\n", "svc/Pipfile": ""},
			want:  []string{"dependency-updates-missing@svc/Pipfile:1"},
		},
		{
			name:  "dependabot only",
			files: map[string]string{"go.mod": "module x\n", ".github/dependabot.yml": "version: 2\n"},
		},
		{
			name:  "no manifests",
			files: map[string]string{"README.md": "# x\n"},
		},
		{
			name:  "renovate key in package.json",
			files: map[string]string{"package.json": `{"name": "x", "renovate": {"extends": ["config:best-practices"]}}`},
		},
		{
			name:  "gitlab directory config",
			files: map[string]string{"go.mod": "module x\n", ".gitlab/renovate.json5": "{extends: ['config:recommended']}"},
		},
		{
			name:  "invalid config",
			files: map[string]string{"go.mod": "module x\n", "renovate.json": "{\n  \"extends\": [\"config:recommended\"]\n  \"automerge\": true\n}\n"},
			want:  []string{"renovate-config-invalid@renovate.json:3"},
		},
		{
			name:  "config is not an object",
			files: map[string]string{"go.mod": "module x\n", ".renovaterc": `["config:recommended"]`},
			want:  []string{"renovate-config-invalid@.renovaterc:1"},
		},
		{
			name:  "disabled",
			files: map[string]string{"go.mod": "module x\n", "renovate.json": "{\n  \"enabled\": false,\n  \"automerge\": true\n}"},
			want:  []string{"renovate-disabled@renovate.json:2"},
		},
		{
			name: "package rule disables everything",
			files: map[string]string{"go.mod": "module x\n", "renovate.json5": `{
  packageRules: [
    {matchPackageNames: ['sentinel-legacy-sdk'], enabled: false},
    {matchPackagePatterns: ['*'], enabled: false},
    {enabled: false},
  ],
}`},
			want: []string{"renovate-disabled@renovate.json5:4", "renovate-disabled@renovate.json5:5"},
		},
		{
			name:  "vulnerability alerts off",
			files: map[string]string{"go.mod": "module x\n", "renovate.json": "{\n  \"vulnerabilityAlerts\": {\n    \"enabled\": false\n  }\n}"},
			want:  []string{"renovate-vulnerability-alerts-off@renovate.json:3"},
		},
		{
			name: "automerge without release age",
			files: map[string]string{"go.mod": "module x\n", ".github/renovate.json": `{
  "extends": ["config:recommended", ":automergeMinor"],
  "packageRules": [
    {"matchUpdateTypes": ["patch"], "automerge": true},
    {"matchUpdateTypes": ["minor"], "automerge": true, "minimumReleaseAge": "3 days"}
  ],
  "lockFileMaintenance": {"enabled": true, "automerge": true}
}`},
			want: []string{
				"renovate-automerge-no-release-age@.github/renovate.json:2",
				"renovate-automerge-no-release-age@.github/renovate.json:4",
				"renovate-automerge-no-release-age@.github/renovate.json:7",
			},
		},
		{
			name:  "automerge with a global release age",
			files: map[string]string{"go.mod": "module x\n", "renovate.json": `{"automerge": true, "minimumReleaseAge": "3 days", "packageRules": [{"automerge": true}]}`},
		},
		{
			name:  "automerge with legacy stabilityDays",
			files: map[string]string{"go.mod": "module x\n", "renovate.json": `{"automerge": true, "stabilityDays": 3}`},
		},
		{
			name:  "zero release age does not count",
			files: map[string]string{"go.mod": "module x\n", "renovate.json": `{"automerge": true, "minimumReleaseAge": "0 days"}`},
			want:  []string{"renovate-automerge-no-release-age@renovate.json:1"},
		},
		{
			name: "actions and images without digest pinning",
			files: map[string]string{
				".github/workflows/ci.yml": workflow, "Dockerfile": "FROM golang:1.27\n",
				"renovate.json": "{\n  \"extends\": [\"config:recommended\"]\n}",
			},
			want: []string{"renovate-no-digest-pinning@renovate.json:2"},
		},
		{
			name: "best practices pins digests",
			files: map[string]string{
				".github/workflows/ci.yml": workflow, "Dockerfile": "FROM golang:1.27\n",
				"renovate.json": `{"extends": ["config:best-practices"], "automerge": true}`,
			},
		},
		{
			name: "explicit digest presets",
			files: map[string]string{
				".github/workflows/ci.yml": workflow, "compose.yaml": "services: {}\n",
				"renovate.json": `{"extends": ["helpers:pinGitHubActionDigests", "docker:pinDigests"]}`,
			},
		},
		{
			name: "docker managers not enabled",
			files: map[string]string{
				"Dockerfile":    "FROM golang:1.27\n",
				"renovate.json": `{"enabledManagers": ["gomod"]}`,
			},
		},
		{
			name: "shared preset may set the rest",
			files: map[string]string{
				".github/workflows/ci.yml": workflow,
				"renovate.json":            `{"extends": ["github>acme/renovate-config"], "automerge": true}`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for rel, content := range tt.files {
				p := filepath.Join(root, filepath.FromSlash(rel))
				require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
				require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
			}
			got := dependencyFindings(t, root)
			slices.Sort(got)
			want := append([]string{}, tt.want...)
			slices.Sort(want)
			assert.Equal(t, got, want)
		})
	}
}

func TestRenovateSymlinkIgnored(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "renovate.json")
	require.NoError(t, os.WriteFile(outside, []byte(`{}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "renovate.json")))
	assert.Equal(t, dependencyFindings(t, root), []string{"dependency-updates-missing@go.mod:1"})
}

func TestRenovateFindingsHoldNoValues(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "renovate.json"), []byte(
		`{"packageRules": [{"matchPackageNames": ["sentinel-pkg"], "automerge": true, "enabled": false}, {"enabled": false}]}`), 0o600))
	g, err := Extract(context.Background(), "r", root)
	require.NoError(t, err)
	n := 0
	for _, f := range g.Nodes() {
		if f.Kind == model.KindFinding {
			n++
			assert.NotContains(t, f.Summary+f.Name, "sentinel-pkg")
		}
	}
	assert.Equal(t, n, 2)
	// Findings hang off the repository: the JSON file has no node.
	for _, e := range g.Edges() {
		if e.Kind == model.EdgeHasFinding {
			assert.Equal(t, e.From, model.NodeID("r", model.KindRepository, "r"))
		}
	}
}

func dependencyFindings(t *testing.T, root string) []string {
	t.Helper()
	g, err := Extract(context.Background(), "r", root)
	require.NoError(t, err)
	out := []string{}
	for _, n := range g.Nodes() {
		if n.Kind == model.KindFinding && (n.Rule == "dependency-updates-missing" || strings.HasPrefix(n.Rule, "renovate-")) {
			out = append(out, fmt.Sprintf("%s@%s:%d", n.Rule, n.File, n.StartLine))
		}
	}
	return out
}
