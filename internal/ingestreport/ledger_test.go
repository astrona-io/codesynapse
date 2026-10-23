package ingestreport

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		p := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0700))
		require.NoError(t, os.WriteFile(p, []byte(content), 0600))
	}
	return root
}
func ledgerFor(t *testing.T, root string, policy *repoconfig.Policy) (*Ledger, context.Context) {
	t.Helper()
	l, err := New("test-run", time.Now())
	require.NoError(t, err)
	ctx := WithLedger(context.Background(), l)
	require.NoError(t, l.Discover(ctx, root, policy))
	return l, ctx
}
func fileByPath(t *testing.T, r Report, path string) File {
	t.Helper()
	for _, f := range r.Files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("missing file %s", path)
	return File{}
}
func TestDiscoveryMetadataAndPruning(t *testing.T) {
	root := fixture(t, map[string]string{"ignored.md": "12345678", "opaque.dat": "opaque", "pruned/a.md": "unseen", "visible.md": "# visible", ".git/config": "private"})
	require.NoError(t, os.Symlink(filepath.Join(root, "visible.md"), filepath.Join(root, "link.md")))
	p, err := repoconfig.Parse([]byte("version: 1\nignore: {paths: [ignored.md, opaque.dat, 'pruned/**']}"))
	require.NoError(t, err)
	l, _ := ledgerFor(t, root, p)
	r := l.Snapshot()
	require.NoError(t, r.Validate())
	assert.Equal(t, r.TotalFilesScanned, 4)
	assert.Equal(t, r.PrunedDirectories, 2)
	assert.Equal(t, r.FilesSkipped, 2)
	assert.Equal(t, r.Estimate.UnknownFiles, 1)
	require.True(t, r.EstimatedTokensSavedOnIngest != nil)
	assert.Equal(t, *r.EstimatedTokensSavedOnIngest, int64(2))
	assert.True(t, r.Estimate.Partial)
	assert.Equal(t, fileByPath(t, r, "link.md").Diagnostic, "symlink")
	clone := r.Clone()
	clone.Files[0].Path = "changed"
	clone.SkipBreakdown[IgnoredPath] = 999
	assert.Equal(t, l.Snapshot().SkipBreakdown[IgnoredPath], 2)
	assert.NotEqual(t, l.Snapshot().Files[0].Path, "changed")
}
func TestCommitAcknowledgementAndRequiredStages(t *testing.T) {
	for _, tc := range []struct {
		name            string
		status          Status
		commit          GraphCommit
		indexed, failed int
	}{
		{"committed", Succeeded, CommitCommitted, 1, 1}, {"write_failed", Failed, CommitFailed, 0, 2}, {"unknown", Failed, CommitUnknown, 0, 1}, {"cancelled", Cancelled, CommitNone, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := fixture(t, map[string]string{"good.yaml": "kind: ConfigMap", "bad.yaml": "[broken", "unsupported.dat": "opaque"})
			l, ctx := ledgerFor(t, root, nil)
			Observe(ctx, "good.yaml", Config, StageSucceeded, "")
			Observe(ctx, "good.yaml", Kubernetes, StageExcluded, "stage_excluded")
			Observe(ctx, "bad.yaml", Config, StageFailed, "parse_failed")
			Observe(ctx, "bad.yaml", Config, StageSucceeded, "")
			g := model.NewGraph()
			for _, name := range []string{"good.yaml", "bad.yaml"} {
				g.AddNode(model.Node{ID: name, Kind: model.KindConfig, File: name})
			}
			prepared := l.Prepare(g)
			require.NoError(t, prepared.Validate())
			assert.Equal(t, prepared.FilesIndexed, 0)
			r := l.Finish(tc.status, tc.commit)
			require.NoError(t, r.Validate())
			assert.Equal(t, r.FilesIndexed, tc.indexed)
			assert.Equal(t, r.FilesFailed, tc.failed)
			assert.Equal(t, r.TotalFilesScanned, r.FilesIndexed+r.FilesSkipped+r.FilesFailed+r.FilesUnprocessed)
			assert.Equal(t, r.FilesWritten, r.FilesIndexed)
			assert.Equal(t, r.FilesReused, 0)
			assert.Equal(t, fileByPath(t, r, "bad.yaml").Outcome, FileFailed)
			assert.Equal(t, fileByPath(t, r, "unsupported.dat").Outcome, Unprocessed)
			again, err := r.Finish(tc.status, tc.commit)
			require.NoError(t, err)
			assert.Equal(t, *again.FinishedAt, *r.FinishedAt)
		})
	}
}
func TestMissingEvidenceAndSkipPrecedence(t *testing.T) {
	root := fixture(t, map[string]string{"raw.go": "package broken", "known.md": "# known"})
	l, ctx := ledgerFor(t, root, nil)
	Skip(ctx, "raw.go", Binary)
	Skip(ctx, "raw.go", GeneratedCode)
	Skip(ctx, "raw.go", IgnoredPath)
	Skip(ctx, "raw.go", NonKubernetesYAML)
	g := model.NewGraph()
	g.AddNode(model.Node{ID: "raw", Kind: model.KindFile, File: "raw.go"})
	g.AddNode(model.Node{ID: "known", Kind: model.KindDocument, File: "known.md"})
	l.Prepare(g)
	r := l.Finish(Succeeded, CommitCommitted)
	require.NoError(t, r.Validate())
	assert.Equal(t, r.FilesIndexed, 0)
	assert.Equal(t, r.FilesSkipped, 1)
	assert.Equal(t, r.FilesUnprocessed, 1)
	assert.Equal(t, fileByPath(t, r, "raw.go").SkipReason, IgnoredPath)
}
func TestCheckpointAndCancellation(t *testing.T) {
	root := fixture(t, map[string]string{"a.md": "# A"})
	l, ctx := ledgerFor(t, root, nil)
	want := errors.New("store unavailable")
	called := false
	l.SetCheckpoint(func(_ context.Context, r Report) error {
		called = true
		assert.Equal(t, r.Status, Running)
		assert.Equal(t, r.TotalFilesScanned, 1)
		return want
	})
	assert.True(t, errors.Is(l.Checkpoint(ctx), want))
	assert.True(t, called)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	other, err := New("cancelled-run", time.Now())
	require.NoError(t, err)
	assert.True(t, errors.Is(other.Discover(cancelled, root, nil), context.Canceled))
	r := other.Finish(Cancelled, CommitNone)
	require.NoError(t, r.Validate())
	assert.False(t, r.DiscoveryComplete)
}
func TestValidateRejectsContradictions(t *testing.T) {
	root := fixture(t, map[string]string{"x.md": "# x"})
	l, ctx := ledgerFor(t, root, nil)
	Contribute(ctx, "x.md", Docs)
	prepared := l.Prepare(nil)
	mutations := map[string]func(*Report){
		"false_commit":               func(r *Report) { r.GraphCommit = CommitCommitted },
		"false_totals":               func(r *Report) { r.FilesIndexed = 1 },
		"false_estimate":             func(r *Report) { n := int64(100); r.EstimatedTokensSavedOnIngest = &n },
		"unsafe_path":                func(r *Report) { r.Files[0].Path = "../outside" },
		"diagnostic_content":         func(r *Report) { r.Files[0].Diagnostic = "secret parser text" },
		"source_control":             func(r *Report) { r.Revision = "abc\n" },
		"invalid_utf":                func(r *Report) { r.PolicyDigest = string([]byte{255}) },
		"reuse_without_contribution": func(r *Report) { r.Files[0].Contribution = false; r.Files[0].Reused = true },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			r := prepared.Clone()
			mutate(&r)
			require.Error(t, r.Validate())
			_, err := r.Finish(Succeeded, CommitCommitted)
			require.Error(t, err)
		})
	}
}
func TestDiscoveryBound(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < MaxFiles+1; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("%05d.dat", i)), nil, 0600))
	}
	l, _ := ledgerFor(t, root, nil)
	r := l.Finish(Failed, CommitNone)
	require.NoError(t, r.Validate())
	assert.Equal(t, r.TotalFilesScanned, MaxFiles)
	assert.False(t, r.DiscoveryComplete)
}
func BenchmarkReportValidate(b *testing.B) {
	l, err := New("benchmark", time.Now())
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		l.report.Files = append(l.report.Files, File{Path: fmt.Sprintf("src/%d.go", i), Outcome: Unprocessed, Diagnostic: "no_contribution"})
	}
	r := l.Snapshot()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := r.Validate(); err != nil {
			b.Fatal(err)
		}
	}
}
func FuzzValidPath(f *testing.F) {
	for _, s := range []string{"a.go", "dir/a.md", "../outside", "a\\b", "a\x00b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if ValidPath(s) {
			assert.False(t, strings.HasPrefix(s, "/"))
			assert.False(t, strings.Contains(s, "\\"))
			assert.True(t, len(s) <= 1024)
		}
	})
}

func TestEstimateUnknownAndBinaryExclusion(t *testing.T) {
	root := fixture(t, map[string]string{"asset.bin": "bytes", "candidate.md": "12345678"})
	l, ctx := ledgerFor(t, root, nil)
	Skip(ctx, "asset.bin", Binary)
	r := l.Snapshot()
	require.NoError(t, r.Validate())
	assert.True(t, r.EstimatedTokensSavedOnIngest == nil)
	assert.Equal(t, r.Estimate.ExcludedBinaryFiles, 1)
	Skip(ctx, "candidate.md", IgnoredPath)
	r = l.Snapshot()
	require.NoError(t, r.Validate())
	require.True(t, r.EstimatedTokensSavedOnIngest != nil)
	assert.Equal(t, *r.EstimatedTokensSavedOnIngest, int64(2))
}
func TestSourceIdentityAndPrematurePublication(t *testing.T) {
	l, err := New("pinned", time.Now())
	require.NoError(t, err)
	require.NoError(t, l.SetSource("revision", "policy-a"))
	require.Error(t, l.SetSource("revision", "policy-b"))
	_, err = l.Snapshot().Finish(Succeeded, CommitCommitted)
	require.Error(t, err)
}
