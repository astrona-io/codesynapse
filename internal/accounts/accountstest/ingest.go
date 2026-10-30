package accountstest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/ingestreport"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// RunIngest checks durable accounting, recovery, cardinality and replay behavior.
func RunIngest(t *testing.T, st accounts.Store) {
	ctx := t.Context()
	svc := accounts.NewService(st, true)
	user, org, err := svc.SignUp(ctx, "ingestion-owner@example.com", "Ingestion owner", pw)
	require.NoError(t, err)
	p := accounts.Principal{User: user, Via: "session"}
	other, err := svc.CreateOrg(ctx, p, "Ingestion other", "")
	require.NoError(t, err)
	graph := "ingestion/private-repo"
	add := func(graph string) {
		require.NoError(t, st.AddRepository(ctx, accounts.Repository{OrgID: org.ID, Graph: graph, AddedBy: user.ID, AddedAt: time.Now(), Visibility: accounts.Private}))
	}
	add(graph)
	id, err := svc.RegisterExecutionSession(ctx, user.ID, org.ID, "ingestion-contract-session")
	require.NoError(t, err)
	work, err := execution.With(ctx, id)
	require.NoError(t, err)
	newRun := func(name string) (accounts.IngestRun, *ingestreport.Ledger) {
		l, err := ingestreport.New(name, time.Now().UTC())
		require.NoError(t, err)
		return accounts.IngestRun{OrgID: org.ID, Graph: graph, Identity: id, Report: l.Snapshot()}, l
	}
	run, l := newRun("first")
	require.Error(t, st.BeginIngestRun(ctx, run))
	require.NoError(t, st.BeginIngestRun(work, run))
	require.NoError(t, st.BeginIngestRun(work, run))
	queued, err := st.GetIngestRun(ctx, org.ID, graph, "first")
	require.NoError(t, err)
	assert.True(t, queued.MetricCoverage)
	assert.Equal(t, ingestFigures(t, st, org.ID).Running, 1)
	assert.Equal(t, len(queued.RepositoryID), 24)
	encoded, err := json.Marshal(queued)
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(encoded), id.SessionID))
	_, err = st.GetIngestRun(ctx, other.ID, graph, "first")
	assert.True(t, errors.Is(err, accounts.ErrNotFound))
	another, _ := newRun("concurrent")
	assert.True(t, errors.Is(st.BeginIngestRun(work, another), accounts.ErrIngestLimit))
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "skip.txt"), []byte("ignored metadata"), 0600))
	require.NoError(t, l.Discover(ctx, root, nil))
	run.Report = l.Snapshot()
	require.NoError(t, st.SaveIngestRun(work, run))
	require.NoError(t, st.SaveIngestRun(work, run))
	checkpoint, err := st.GetIngestRun(ctx, org.ID, graph, "first")
	require.NoError(t, err)
	assert.Equal(t, checkpoint.Report.TotalFilesScanned, 2)
	ingestreport.Skip(ingestreport.WithLedger(ctx, l), "skip.txt", ingestreport.IgnoredPath)
	ingestreport.Contribute(ingestreport.WithLedger(ctx, l), "go.mod", ingestreport.Dependencies)
	run.Report = l.Prepare(nil)
	require.NoError(t, st.SaveIngestRun(work, run))
	prepared := run
	changed := run
	changed.Report = run.Report.Clone()
	changed.Report.Files[0].Stages[0].Stage = ingestreport.Docs
	require.Error(t, st.SaveIngestRun(work, changed))
	var initial accounts.IngestMetricSnapshot
	require.NoError(t, st.ExportIngestTotals(ctx, func(_ context.Context, s accounts.IngestMetricSnapshot) error { initial = s; return nil }))
	assert.Equal(t, ingestTotal(initial, queued.RepositoryID).Scanned, int64(2))
	assert.Equal(t, ingestTotal(initial, queued.RepositoryID).Written, int64(0))
	assert.Equal(t, ingestTotal(initial, queued.RepositoryID).Skipped[ingestreport.IgnoredPath], int64(1))
	run.Report = l.Finish(ingestreport.Succeeded, ingestreport.CommitCommitted)
	require.NoError(t, run.Report.Validate())
	rollbackError := errors.New("rollback publication")
	err = st.PolicyTransaction(work, org.ID, func(tx accounts.Store) error {
		if err := tx.SaveIngestRun(work, run); err != nil {
			return err
		}
		return rollbackError
	})
	assert.True(t, errors.Is(err, rollbackError))
	beforeCommit, err := st.GetIngestRun(ctx, org.ID, graph, "first")
	require.NoError(t, err)
	assert.Equal(t, beforeCommit.Report.Status, ingestreport.Prepared)
	require.NoError(t, st.ExportIngestTotals(ctx, func(_ context.Context, s accounts.IngestMetricSnapshot) error {
		assert.Equal(t, ingestTotal(s, queued.RepositoryID).Written, int64(0))
		return nil
	}))
	// Multiple acknowledgements race on the same final report, not on new attempts.
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Go(func() { results <- st.SaveIngestRun(work, run) })
	}
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	var exported accounts.IngestMetricSnapshot
	require.NoError(t, st.ExportIngestTotals(ctx, func(_ context.Context, s accounts.IngestMetricSnapshot) error { exported = s; return nil }))
	total := ingestTotal(exported, queued.RepositoryID)
	assert.Equal(t, total.Scanned, int64(2))
	assert.Equal(t, total.Written, int64(1))
	assert.Equal(t, total.Skipped[ingestreport.IgnoredPath], int64(1))
	assert.Equal(t, exported.StreamID, initial.StreamID)
	assert.True(t, exported.SampleAt.After(initial.SampleAt))
	stored, err := st.GetIngestRun(ctx, org.ID, graph, "first")
	require.NoError(t, err)
	assert.Equal(t, stored.LastExportedAt, exported.SampleAt)
	stored.Report.Files[0].Path = "modified"
	stored.Report.SkipBreakdown[ingestreport.IgnoredPath] = 999
	stored, err = st.GetIngestRun(ctx, org.ID, graph, "first")
	require.NoError(t, err)
	assert.Equal(t, stored.Report.Files[0].Path, "go.mod")
	assert.Equal(t, stored.Report.SkipBreakdown[ingestreport.IgnoredPath], 1)
	figures := ingestFigures(t, st, org.ID)
	assert.Equal(t, figures.Succeeded, 1)
	assert.Equal(t, figures.Running, 0)
	assert.Equal(t, figures.Scanned, 2)
	assert.Equal(t, figures.Indexed, stored.Report.FilesIndexed)
	assert.Equal(t, figures.Skipped, 1)
	assert.Equal(t, figures.Skips.IgnoredPath, 1)
	assert.Equal(t, figures.IncompleteDiscovery, 0)
	past, err := st.IngestStats(ctx, time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
	require.NoError(t, err)
	assert.Len(t, past, 0)
	assert.True(t, errors.Is(st.SaveIngestRun(work, prepared), accounts.ErrIngestConflict))
	// Export failure preserves the last confirmed sample and cannot run two owners.
	offline := errors.New("export unavailable")
	err = st.ExportIngestTotals(ctx, func(_ context.Context, s accounts.IngestMetricSnapshot) error {
		assert.True(t, s.SampleAt.After(exported.SampleAt))
		assert.True(t, errors.Is(st.ExportIngestTotals(ctx, func(context.Context, accounts.IngestMetricSnapshot) error { return nil }), accounts.ErrIngestExporterBusy))
		return offline
	})
	assert.True(t, errors.Is(err, offline))
	stored, err = st.GetIngestRun(ctx, org.ID, graph, "first")
	require.NoError(t, err)
	assert.Equal(t, stored.LastExportedAt, exported.SampleAt)
	// A policy transaction rollback must roll back both accounting and its audit.
	rollback, _ := newRun("rollback")
	err = st.PolicyTransaction(work, org.ID, func(tx accounts.Store) error {
		if err := tx.BeginIngestRun(work, rollback); err != nil {
			return err
		}
		return offline
	})
	assert.True(t, errors.Is(err, offline))
	_, err = st.GetIngestRun(ctx, org.ID, graph, "rollback")
	assert.True(t, errors.Is(err, accounts.ErrNotFound))
	// Keep the latest success while pruning failed attempts and retaining dedupe.
	for i := range accounts.IngestHistory + 2 {
		failed, ledger := newRun(fmt.Sprintf("failure-%03d", i))
		require.NoError(t, st.BeginIngestRun(work, failed))
		failed.Report = ledger.Finish(ingestreport.Failed, ingestreport.CommitNone)
		require.NoError(t, st.SaveIngestRun(work, failed))
	}
	history, err := st.ListIngestRuns(ctx, org.ID, graph, 10000)
	require.NoError(t, err)
	assert.Len(t, history, accounts.IngestHistory+1)
	// Expired reports still count their status, by their start.
	figures = ingestFigures(t, st, org.ID)
	assert.Equal(t, figures.Succeeded, 1)
	assert.Equal(t, figures.Failed, accounts.IngestHistory+2)
	assert.Equal(t, figures.WithoutReport, 2)
	assert.Equal(t, figures.IncompleteDiscovery, accounts.IngestHistory)
	assert.Equal(t, figures.Scanned, 2)
	for _, summary := range history {
		assert.Len(t, summary.Report.Files, 0)
	}
	latest, err := st.LatestSuccessfulIngestRun(ctx, org.ID, graph)
	require.NoError(t, err)
	assert.Equal(t, latest.Report.RunID, "first")
	assert.Len(t, latest.Report.Files, 0)
	_, err = st.GetIngestRun(ctx, org.ID, graph, "failure-000")
	assert.True(t, errors.Is(err, accounts.ErrIngestReportExpired))
	replay, _ := newRun("failure-000")
	assert.True(t, errors.Is(st.BeginIngestRun(work, replay), accounts.ErrIngestConflict))
	// Recovery can account a committed prepared receipt after session revocation.
	recovery, ledger := newRun("recovery")
	require.NoError(t, st.BeginIngestRun(work, recovery))
	require.NoError(t, ledger.Discover(ctx, root, nil))
	ingestreport.Contribute(ingestreport.WithLedger(ctx, ledger), "go.mod", ingestreport.Dependencies)
	recovery.Report = ledger.Prepare(nil)
	require.NoError(t, st.SaveIngestRun(work, recovery))
	require.NoError(t, svc.RevokeExecutionSession(ctx, user.ID, org.ID, id.SessionID))
	recovery.Report = ledger.Finish(ingestreport.Succeeded, ingestreport.CommitCommitted)
	require.Error(t, st.SaveIngestRun(work, recovery))
	require.NoError(t, st.ReconcileIngestRun(ctx, org.ID, graph, "recovery", ingestreport.CommitCommitted))
	require.NoError(t, st.ReconcileIngestRun(ctx, org.ID, graph, "recovery", ingestreport.CommitCommitted))
	pending, err := st.PendingIngestRuns(ctx, graph, 1000)
	require.NoError(t, err)
	assert.Len(t, pending, 0)
	var recovered accounts.IngestMetricSnapshot
	require.NoError(t, st.ExportIngestTotals(ctx, func(_ context.Context, s accounts.IngestMetricSnapshot) error { recovered = s; return nil }))
	assert.Equal(t, ingestTotal(recovered, queued.RepositoryID).Written, int64(2))
	// Unlinking hides payloads; attaching again retains the accounting identity.
	_, err = st.RemoveRepository(ctx, org.ID, graph)
	require.NoError(t, err)
	history, err = st.ListIngestRuns(ctx, org.ID, graph, 100)
	require.NoError(t, err)
	assert.Len(t, history, 0)
	add(graph)
	id, err = svc.RegisterExecutionSession(ctx, user.ID, org.ID, "ingestion-relinked-session")
	require.NoError(t, err)
	work, err = execution.With(ctx, id)
	require.NoError(t, err)
	relink, ledger := newRun("relinked")
	require.NoError(t, st.BeginIngestRun(work, relink))
	relink.Report = ledger.Finish(ingestreport.Cancelled, ingestreport.CommitNone)
	require.NoError(t, st.SaveIngestRun(work, relink))
	stored, err = st.GetIngestRun(ctx, org.ID, graph, "relinked")
	require.NoError(t, err)
	assert.Equal(t, stored.RepositoryID, queued.RepositoryID)
	// Admission slots survive unlinking. Exact reports remain available at overflow.
	for i := 1; i <= accounts.MaxTenantIngestMetricRepositories; i++ {
		graph = fmt.Sprintf("ingestion/admission-%03d", i)
		add(graph)
		admission, ledger := newRun("admission")
		require.NoError(t, st.BeginIngestRun(work, admission))
		admission.Report = ledger.Finish(ingestreport.Failed, ingestreport.CommitNone)
		require.NoError(t, st.SaveIngestRun(work, admission))
		stored, err = st.GetIngestRun(ctx, org.ID, graph, "admission")
		require.NoError(t, err)
		assert.Equal(t, stored.MetricCoverage, i < accounts.MaxTenantIngestMetricRepositories)
	}
	require.NoError(t, st.ExportIngestTotals(ctx, func(_ context.Context, s accounts.IngestMetricSnapshot) error {
		assert.Equal(t, s.DroppedSeries, int64(6))
		count := 0
		for _, total := range s.Totals {
			if total.TenantID == org.ID {
				count++
			}
		}
		assert.Equal(t, count, accounts.MaxTenantIngestMetricRepositories)
		return nil
	}))
	verification, err := accounts.VerifyAudit(ctx, st)
	require.NoError(t, err)
	assert.True(t, verification.OK)
}

// ingestFigures are org's ingest figures of the last and next hour.
func ingestFigures(t *testing.T, st accounts.Store, org string) accounts.IngestFigures {
	t.Helper()
	rows, err := st.IngestStats(t.Context(), time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	for _, r := range rows {
		if r.OrgID == org {
			assert.True(t, r.OrgExists)
			return r.IngestFigures
		}
	}
	t.Fatalf("no ingest figures for the organisation")
	return accounts.IngestFigures{}
}

func ingestTotal(snapshot accounts.IngestMetricSnapshot, id string) accounts.IngestMetricTotal {
	for _, total := range snapshot.Totals {
		if total.RepositoryID == id {
			return total
		}
	}
	return accounts.IngestMetricTotal{}
}
