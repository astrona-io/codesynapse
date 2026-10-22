package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestKubernetesExclusionKeepsGenericSecurity(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "path exclusions", true: "scanner disabled"}[disabled], func(t *testing.T) {
			root := t.TempDir()
			yaml := "apiVersion: v1\nkind: Pod\nmetadata:\n  name: app\npassword: actual-secret-value\nspec:\n  hostNetwork: true\n  containers:\n    - name: app\n      image: nginx\n      securityContext:\n        privileged: true\n"
			files := map[string]string{"ci/app.yaml": yaml, "chart/Chart.yaml": "apiVersion: v2\nname: app\nversion: 1.0.0\n", "chart/templates/app.yaml": yaml, "normal.yaml": "password: actual-secret-value\n"}
			for rel, data := range files {
				p := filepath.Join(root, rel)
				require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
				require.NoError(t, os.WriteFile(p, []byte(data), 0o600))
			}
			config := "version: 1\nscanners:\n  kubernetes:\n    ignore_paths: [ci/**, chart/templates/**]\n"
			if disabled {
				config = "version: 1\nscanners:\n  kubernetes:\n    enabled: false\n"
			}
			policy, err := repoconfig.Parse([]byte(config))
			require.NoError(t, err)
			g, err := Extract(repoconfig.WithPolicy(context.Background(), policy), "r", root)
			require.NoError(t, err)
			secretFiles := map[string]bool{}
			keys := false
			for _, n := range g.Nodes() {
				if n.Kind == model.KindFinding {
					assert.False(t, strings.HasPrefix(n.Rule, "k8s-"))
					if n.Rule == "yaml-hardcoded-secret" {
						secretFiles[n.File] = true
					}
				}
				if n.File == "ci/app.yaml" && n.Kind == model.KindConfigEntry && n.Name == "apiVersion" {
					keys = true
				}
				assert.False(t, strings.HasPrefix(n.Summary, "Kubernetes:"))
			}
			assert.True(t, keys)
			for _, rel := range []string{"ci/app.yaml", "chart/templates/app.yaml", "normal.yaml"} {
				assert.True(t, secretFiles[rel], "missing generic secret check for %s", rel)
			}
		})
	}
}
