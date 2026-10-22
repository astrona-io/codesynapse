package snapshot

import (
	"context"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/cleanup"
	"github.com/parisnakitakejser/codesynapse/internal/duplicates"
	"github.com/parisnakitakejser/codesynapse/internal/license"
	"github.com/parisnakitakejser/codesynapse/internal/metrics"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/store/memory"
	"github.com/parisnakitakejser/codesynapse/internal/store/storetest"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func repoWithReports(t *testing.T) (*memory.Store, *model.Graph) {
	t.Helper()
	g := storetest.Fixture()
	rn, ok := g.Node(storetest.ID(model.KindRepository, storetest.Repo))
	require.True(t, ok)
	cl, err := cleanup.Encode(cleanup.Report{Summary: cleanup.Summary{Functions: 40, Unreachable: 3, TestOnly: 2, DeadLOC: 90, UnusedModules: 1}})
	require.NoError(t, err)
	dup, err := duplicates.Encode(duplicates.Report{Summary: duplicates.Summary{RedundantLOC: 60, NearRedundantLOC: 20, FunctionLOC: 1000, Percent: 8}})
	require.NoError(t, err)
	lic, err := license.Encode(license.Report{Conflict: 1, Review: 4})
	require.NoError(t, err)
	met, err := metrics.Encode(metrics.Report{Tests: metrics.Tests{ReachablePct: 61.5}, Docs: metrics.Docs{CoveragePct: 33}})
	require.NoError(t, err)
	rn.Cleanup, rn.Duplicates, rn.Licenses, rn.Metrics = cl, dup, lic, met
	s := memory.New()
	require.NoError(t, s.ReplaceRepo(context.Background(), storetest.Repo, g))
	return s, g
}

func TestTakeReadsTheStoredReports(t *testing.T) {
	ctx := context.Background()
	s, _ := repoWithReports(t)
	snap, err := Take(ctx, s, storetest.Repo)
	require.NoError(t, err)
	assert.Equal(t, snap.Head, "abc")
	assert.Equal(t, snap.At, "2026-01-01T00:00:00Z", "the ingest time identifies the snapshot")
	assert.True(t, snap.Grade != "", "the security report is taken from the graph")
	assert.Equal(t, snap.DeadFunctions, 5)
	assert.Equal(t, snap.DeadLOC, 90)
	assert.Equal(t, snap.UnusedModules, 1)
	assert.Equal(t, snap.Functions, 40)
	assert.Equal(t, snap.DupLOC, 60)
	assert.Equal(t, snap.NearLOC, 20)
	assert.Equal(t, snap.DupPct, 8.0)
	assert.Equal(t, snap.FunctionLOC, 1000)
	assert.Equal(t, snap.LicenceConflicts, 1)
	assert.Equal(t, snap.LicenceReview, 4)
	assert.Equal(t, snap.TestReachPct, 61.5)
	assert.Equal(t, snap.DocCoveragePct, 33.0)

	_, err = Take(ctx, s, "nope")
	assert.Error(t, err)
}

func TestRecordKeepsHistoryAndNeverFails(t *testing.T) {
	ctx := context.Background()
	s, g := repoWithReports(t)
	Record(ctx, s, storetest.Repo)
	got, err := s.Snapshots(ctx, storetest.Repo, 10)
	require.NoError(t, err)
	require.Len(t, got, 1)

	// A later ingest has another time and adds a second point.
	rn, _ := g.Node(storetest.ID(model.KindRepository, storetest.Repo))
	rn.Time = "2026-02-01T00:00:00Z"
	require.NoError(t, s.ReplaceRepo(ctx, storetest.Repo, g))
	Record(ctx, s, storetest.Repo)
	got, err = s.Snapshots(ctx, storetest.Repo, 10)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, got[1].At, "2026-02-01T00:00:00Z")

	// An unknown repository is logged and dropped, not a panic or an error.
	Record(ctx, s, "nope")
	none, err := s.Snapshots(ctx, "nope", 10)
	require.NoError(t, err)
	assert.Len(t, none, 0)
}
