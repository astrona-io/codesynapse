package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"

	"github.com/astrona-io/codesynapse/internal/ingest"
	agentmem "github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
)

func TestSections(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	b, err := agentmem.Prepare(storetest.Repo, []agentmem.Triple{{Subject: "team/blue", Predicate: "OWNS", Object: "Payments"}}, "t", time.Now())
	require.NoError(t, err)
	require.NoError(t, st.UpsertMemory(ctx, b))
	ts := httptest.NewServer(New(ctx, Config{}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)

	var got Sections
	require.Equal(t, get(t, ts, "/api/v1/repos/"+storetest.Repo+"/sections", &got), 200)
	assert.Equal(t, got.Symbols, 5)
	assert.Equal(t, got.Documents, 1)
	assert.Equal(t, got.ConfigFiles, 2, "a YAML file and a template")
	assert.Equal(t, got.HelmCharts, 1)
	assert.Equal(t, got.Packages, 0, "the fixture has no packages, modules or findings")
	assert.Equal(t, got.Dependencies, 0)
	assert.Equal(t, got.Findings, 0)
	assert.Equal(t, got.MemoryFacts, 1)
	assert.Equal(t, got.Reviews, 0)
	assert.False(t, got.Licences, "the fixture has no licence report")
	assert.True(t, got.Admin, "single-tenant mode administers everything")

	assert.Equal(t, get(t, ts, "/api/v1/repos/nope/sections", nil), 404)
}

// TestSectionsByRepositoryShape indexes three one-sided repositories and
// checks that each has content only where the menu should keep a link.
func TestSectionsByRepositoryShape(t *testing.T) {
	ctx := context.Background()
	shapes := map[string]map[string]string{
		"go-only": {
			"go.mod":  "module example.com/goonly\n\ngo 1.22\n",
			"main.go": "package main\n\nfunc main() { run() }\n\nfunc run() {}\n",
		},
		"helm-only": {
			"chart/Chart.yaml":            "apiVersion: v2\nname: web\nversion: 0.1.0\n",
			"chart/values.yaml":           "replicas: 1\n",
			"chart/templates/deploy.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\nspec:\n  replicas: {{ .Values.replicas }}\n",
		},
		"python-only": {
			"app/__init__.py": "",
			"app/main.py":     "def handler(event):\n    return event\n",
		},
	}
	st := memory.New()
	for name, files := range shapes {
		root := t.TempDir()
		for rel, body := range files {
			p := filepath.Join(root, filepath.FromSlash(rel))
			require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
			require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
		}
		g, err := ingest.Run(ctx, ingest.Options{Repo: name, Root: root})
		require.NoError(t, err)
		require.NoError(t, st.ReplaceRepo(ctx, name, g))
	}
	ts := httptest.NewServer(New(ctx, Config{}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)
	sections := func(repo string) Sections {
		var s Sections
		require.Equal(t, get(t, ts, "/api/v1/repos/"+repo+"/sections", &s), 200)
		return s
	}

	goOnly := sections("go-only")
	assert.Greater(t, goOnly.Packages, 0)
	assert.Equal(t, goOnly.ConfigFiles, 0)
	assert.Equal(t, goOnly.Documents, 0)

	helm := sections("helm-only")
	assert.Equal(t, helm.HelmCharts, 1)
	assert.Greater(t, helm.ConfigFiles, 0)
	assert.Equal(t, helm.Packages, 0)
	assert.Equal(t, helm.Symbols, 0)

	py := sections("python-only")
	assert.Greater(t, py.Symbols, 0)
	assert.Equal(t, py.ConfigFiles, 0)
	assert.Equal(t, py.HelmCharts, 0)
}
