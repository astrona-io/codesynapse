package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/ingest"
	"github.com/astrona-io/codesynapse/internal/ingestreport"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func prepareIngestFixture(t *testing.T, f runtimeFixture, id string) (Job, *ingestreport.Ledger, *model.Graph) {
	t.Helper()
	ctx := runtimeContext(t, f)
	l, err := ingestreport.New(id, time.Now().UTC())
	require.NoError(t, err)
	job := Job{ID: id, Repo: storetest.Repo, OrgID: f.org.ID, Identity: f.identity}
	require.NoError(t, f.server.saveIngestReport(ctx, job, l.Snapshot()))
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("# Hello\nA repository guide.\n"), 0o600))
	g, err := ingest.Run(ingestreport.WithLedger(ctx, l), ingest.Options{Repo: storetest.Repo, Root: root})
	require.NoError(t, err)
	require.NoError(t, f.server.saveIngestReport(ctx, job, l.Prepare(g)))
	return job, l, g
}

func TestIngestReceiptRecoveryAndTenantIsolation(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	ctx := runtimeContext(t, f)
	job, _, graph := prepareIngestFixture(t, f, "receipt-run")
	require.NoError(t, attachIngestReceipt(graph, job.OrgID, job.Repo, job.ID))
	require.NoError(t, f.store.ReplaceRepo(ctx, job.Repo, graph))
	// The process died after graph commit and before its report transaction.
	require.Equal(t, f.client.do("DELETE", "/api/v1/execution/sessions/thread-a", "", nil), 204)
	require.NoError(t, f.server.reconcileIngestReports(t.Context(), job.Repo))
	require.NoError(t, f.server.reconcileIngestReports(t.Context(), job.Repo))
	run, err := f.accounts.St.GetIngestRun(t.Context(), job.OrgID, job.Repo, job.ID)
	require.NoError(t, err)
	assert.Equal(t, run.Report.Status, ingestreport.Succeeded)
	assert.Equal(t, run.Report.FilesWritten, 1)
	var total int64
	require.NoError(t, f.accounts.St.ExportIngestTotals(t.Context(), func(_ context.Context, snapshot accounts.IngestMetricSnapshot) error {
		total = snapshot.Totals[0].Written
		return nil
	}))
	assert.Equal(t, total, int64(1), "receipt replay cannot increment twice")
	path := "/api/v1/repos/" + storetest.Repo + "/ingestion"
	var out map[string]any
	require.Equal(t, f.client.do("GET", path+"/"+job.ID, "", &out), 200)
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(raw), "thread-a"), "repository readers cannot see the originating session")
	assert.True(t, strings.Contains(string(raw), "README.md"))
	u, org, err := f.accounts.SignUp(t.Context(), "ingest-other@example.com", "Other", pw)
	require.NoError(t, err)
	require.NoError(t, f.accounts.St.AddRepository(t.Context(), accounts.Repository{OrgID: org.ID, Graph: storetest.Repo, AddedBy: u.ID, AddedAt: time.Now()}))
	key, _, err := f.accounts.CreateKey(t.Context(), accounts.Principal{User: u, Via: "session"}, org.ID, "reports", 0)
	require.NoError(t, err)
	other := newClient(t, f.client.ts)
	other.auth = "Bearer " + key
	assert.Equal(t, other.do("GET", path+"/"+job.ID, "", nil), 404)
	var history struct {
		History []accounts.IngestRun `json:"history"`
	}
	require.Equal(t, other.do("GET", path, "", &history), 200)
	assert.Len(t, history.History, 0)
}

type ingestWriteFailure struct {
	store.Store
	committed bool
}

func (s ingestWriteFailure) ReplaceRepo(ctx context.Context, repo string, g *model.Graph) error {
	if s.committed {
		if err := s.Store.ReplaceRepo(ctx, repo, g); err != nil {
			return err
		}
	}
	return errors.New("write acknowledgement unavailable")
}

func TestJobsReportGraphAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		name               string
		failure, committed bool
		want               int
	}{
		{"success", false, true, 1}, {"rejected", true, false, 0}, {"acknowledgement lost", true, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRuntimeFixture(t, Config{})
			var writer store.Writer = f.store
			if tc.failure {
				writer = ingestWriteFailure{Store: f.store, committed: tc.committed}
			}
			jobs := NewJobs(t.Context(), writer, f.server.logger, 0, false, false)
			jobs.Report, jobs.Publish, jobs.Policy = f.server.saveIngestReport, f.server.publishRepo, f.server.approveIngestPolicy
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("# A guide\n"), 0o600))
			after := make(chan error, 1)
			job, err := jobs.Start(storetest.Repo, root, JobOptions{Identity: f.identity, OrgID: f.org.ID, Authorize: func(ctx context.Context) error {
				_, err := f.server.accountsFor(ctx).ResolveExecutionSession(ctx, f.user.ID, f.org.ID, f.identity.SessionID)
				return err
			}, After: func(ctx context.Context) {
				identity, err := execution.Require(ctx)
				if err != nil || identity != f.identity {
					after <- errors.New("post-ingest hook lost execution identity")
					return
				}
				select {
				case <-time.After(20 * time.Millisecond):
				case <-ctx.Done():
				}
				after <- ctx.Err()
			}})
			require.NoError(t, err)
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
				current, _ := jobs.Get(job.ID)
				if current.State != JobRunning {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			run, err := f.accounts.St.GetIngestRun(t.Context(), f.org.ID, storetest.Repo, job.ID)
			require.NoError(t, err)
			assert.True(t, run.Report.FinishedAt != nil)
			assert.Equal(t, run.Report.FilesIndexed, tc.want)
			assert.Equal(t, run.Report.TotalFilesScanned, run.Report.FilesIndexed+run.Report.FilesSkipped+run.Report.FilesFailed+run.Report.FilesUnprocessed)
			if !tc.failure {
				select {
				case err := <-after:
					require.NoError(t, err)
				case <-time.After(time.Second):
					t.Fatal("post-ingest hook did not run")
				}
			}
		})
	}
}

func TestIngestRecoveryRequiresOriginalIdentity(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	job, ledger, graph := prepareIngestFixture(t, f, "bound-receipt")
	require.NoError(t, attachIngestReceipt(graph, job.OrgID, job.Repo, job.ID))
	require.NoError(t, f.store.ReplaceRepo(t.Context(), job.Repo, graph))
	report := ledger.Finish(ingestreport.Failed, ingestreport.CommitUnknown)
	job.Identity.SessionID, job.Identity.ParentSessionID = "other-thread", "other-thread"
	assert.ErrorIs(t, f.server.saveIngestReport(runtimeContext(t, f), job, report), execution.ErrConflict)
	ctx, err := execution.With(t.Context(), job.Identity)
	require.NoError(t, err)
	assert.ErrorIs(t, f.server.saveIngestReport(ctx, job, report), execution.ErrConflict)
	run, err := f.accounts.St.GetIngestRun(t.Context(), f.org.ID, job.Repo, job.ID)
	require.NoError(t, err)
	assert.Equal(t, run.Report.Status, ingestreport.Prepared)
}

func TestIngestSeriesRequireRepositoryRead(t *testing.T) {
	vm := &fakeVM{}
	server := httptest.NewServer(vm)
	t.Cleanup(server.Close)
	f := newRuntimeFixture(t, Config{Metrics: &telemetry.Querier{URL: server.URL}})
	job, ledger, _ := prepareIngestFixture(t, f, "metrics-run")
	require.NoError(t, f.server.saveIngestReport(runtimeContext(t, f), job, ledger.Finish(ingestreport.Failed, ingestreport.CommitNone)))
	assert.Equal(t, f.client.do("GET", "/api/v1/stats/series?panel=ingest_files", "", nil), 404)
	assert.Equal(t, f.client.do("GET", "/api/v1/stats/series?panel=ingest_files&repo=foreign", "", nil), 404)
	require.Equal(t, f.client.do("GET", "/api/v1/stats/series?panel=ingest_files&repo="+storetest.Repo, "", nil), 200)
	run, err := f.accounts.St.GetIngestRun(t.Context(), f.org.ID, storetest.Repo, job.ID)
	require.NoError(t, err)
	filter, _ := vm.last()
	assert.Contains(t, filter, f.org.ID)
	assert.Contains(t, filter, run.RepositoryID)
	assert.False(t, strings.Contains(filter, storetest.Repo))
	require.NoError(t, f.accounts.St.SetRepositoryAccess(t.Context(), f.org.ID, storetest.Repo, accounts.AccessLost, time.Now()))
	assert.Equal(t, f.client.do("GET", "/api/v1/stats/series?panel=ingest_files&repo="+storetest.Repo, "", nil), 404)
	assert.Equal(t, f.client.do("GET", "/api/v1/repos/"+storetest.Repo+"/ingestion", "", nil), 404)
}
