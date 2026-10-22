package ingest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/ingestreport"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestIngestReportStagesAndPublication(t *testing.T) {
	root := policyFiles(t, map[string]string{
		".codesynapse.yaml": "version: 1\nignore: {paths: [ignored.md, 'pruned/**']}\nscanners: {kubernetes: {ignore_paths: [app.yaml]}}\n",
		"ignored.md":        "# ignored", "pruned/hidden.js": "function hidden() {}", "app.yaml": "password: actual-secret\n", "broken.yaml": "key: [\n",
		"app.js": "export function run() { return 1; }\n", "README.md": "# Example\n", "opaque.dat": "opaque",
		"package-lock.json": `{"lockfileVersion":3,"packages":{"node_modules/example":{"version":"1.2.3"}}}`,
	})
	p, err := repoconfig.Load(context.Background(), root)
	require.NoError(t, err)
	l, err := ingestreport.New("integration", time.Now())
	require.NoError(t, err)
	ctx := ingestreport.WithLedger(context.Background(), l)
	checkpointed := false
	l.SetCheckpoint(func(_ context.Context, r ingestreport.Report) error {
		checkpointed = true
		assert.Equal(t, r.Status, ingestreport.Running)
		assert.Equal(t, r.FilesIndexed, 0)
		return r.Validate()
	})
	g, err := Run(ctx, Options{Repo: "r", Root: root, ApprovedPolicyDigest: p.Digest()})
	require.NoError(t, err)
	assert.True(t, checkpointed)
	prepared := l.Prepare(g)
	require.NoError(t, prepared.Validate())
	assert.Equal(t, prepared.FilesIndexed, 0)
	r := l.Finish(ingestreport.Succeeded, ingestreport.CommitCommitted)
	require.NoError(t, r.Validate())
	assert.Equal(t, r.TotalFilesScanned, 8)
	assert.Equal(t, r.FilesIndexed, 4)
	assert.Equal(t, r.FilesSkipped, 2)
	assert.Equal(t, r.FilesFailed, 1)
	assert.Equal(t, r.FilesUnprocessed, 1)
	found := false
	for _, file := range r.Files {
		if file.Path == "app.yaml" {
			assert.Equal(t, file.Outcome, ingestreport.Indexed)
			for _, stage := range file.Stages {
				if stage.Stage == ingestreport.Kubernetes {
					found = true
					assert.Equal(t, stage.Outcome, ingestreport.StageExcluded)
				}
			}
		}
		if file.Path == "broken.yaml" {
			assert.Equal(t, file.Diagnostic, "parse_failed")
		}
	}
	assert.True(t, found)
}
func TestCheckpointFailureStopsBeforeReadingSource(t *testing.T) {
	root := policyFiles(t, map[string]string{"app.js": "export function run() {}"})
	l, err := ingestreport.New("checkpoint-failure", time.Now())
	require.NoError(t, err)
	expected := errors.New("checkpoint unavailable")
	l.SetCheckpoint(func(_ context.Context, _ ingestreport.Report) error {
		require.NoError(t, os.Remove(filepath.Join(root, "app.js")))
		return expected
	})
	g, err := Run(ingestreport.WithLedger(context.Background(), l), Options{Repo: "r", Root: root})
	assert.True(t, errors.Is(err, expected))
	assert.True(t, g == nil)
	r := l.Finish(ingestreport.Failed, ingestreport.CommitNone)
	require.NoError(t, r.Validate())
	assert.Equal(t, r.FilesIndexed, 0)
	assert.Equal(t, r.TotalFilesScanned, 1)
}

func TestIngestReportHelmParseFailure(t *testing.T) {
	root := policyFiles(t, map[string]string{"chart/Chart.yaml": "apiVersion: v2\nname: test\nversion: 1.0.0\n", "chart/templates/broken.yaml": "data: [\n"})
	l, err := ingestreport.New("helm-failure", time.Now())
	require.NoError(t, err)
	g, err := Run(ingestreport.WithLedger(context.Background(), l), Options{Repo: "r", Root: root})
	require.NoError(t, err)
	l.Prepare(g)
	r := l.Finish(ingestreport.Succeeded, ingestreport.CommitCommitted)
	require.NoError(t, r.Validate())
	assert.Equal(t, r.FilesFailed, 1)
	assert.Equal(t, r.FilesIndexed, 1)
}
