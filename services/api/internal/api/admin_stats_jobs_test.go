package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/ingestreport"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// jobsVM answers the finished-jobs query with fixed series and every other
// query with nothing.
type jobsVM struct{ series string }

func (v jobsVM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	result := "[]"
	if strings.Contains(r.URL.Query().Get("query"), "codesynapse_jobs_finished_total") {
		result = v.series
	}
	_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":`+result+`}}`)
}

func TestJobsAndIngestSections(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	accSt := acctmem.New()
	acc := accounts.NewService(accSt, false)
	bootstrapAccounts(t, acc)
	acc.SignupOpen = true

	signup := func(ts *httptest.Server, email, name string) (*client, accounts.User, accounts.Org) {
		c := newClient(t, ts)
		var signed struct {
			User accounts.User
			Org  accounts.Org
		}
		require.Equal(t, c.signUp(`{"email":"`+email+`","name":"`+name+`","password":"`+pw+`"}`, &signed), 202)
		return c, signed.User, signed.Org
	}
	const gone = "0123456789abcdef01234567"
	// The VM answer needs organisation IDs, so the server starts with a
	// placeholder and the series are filled in once they exist.
	vmSeries := &jobsVM{}
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { vmSeries.ServeHTTP(w, r) }))
	t.Cleanup(vm.Close)
	srv := New(ctx, Config{Accounts: acc, ServiceToken: "svc-token", Metrics: &telemetry.Querier{URL: vm.URL}}, memory.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	ada, adaUser, _ := signup(ts, "ada@example.com", "Ada")
	require.NoError(t, acc.SetPlatformAdmin(ctx, rootSession(t, acc), adaUser.ID, true))
	member, memberUser, orgA := signup(ts, "a@example.com", "A")
	vmSeries.series = `[
		{"metric":{"tenant_id":"` + orgA.ID + `","kind":"sync","outcome":"ok"},"values":[[1,"4"]]},
		{"metric":{"tenant_id":"` + orgA.ID + `","kind":"sync","outcome":"error"},"values":[[1,"1"]]},
		{"metric":{"tenant_id":"` + gone + `","kind":"clone","outcome":"ok"},"values":[[1,"2"]]},
		{"metric":{"kind":"ingest","outcome":"error"},"values":[[1,"3"]]}]`

	// A job in the queue and one that failed, both naming a repository and
	// an error no platform view may show.
	now := time.Now().UTC()
	exec := execution.Identity{TenantID: orgA.ID, SubjectID: memberUser.ID, SessionID: "web:1", ParentSessionID: "web:1"}
	job := func(id string) accounts.JobRecord {
		return accounts.JobRecord{ID: id, Kind: "sync", Repo: "secret-repo/private", OrgID: orgA.ID, ActorID: memberUser.ID, Via: "session", Role: accounts.Admin,
			Execution: exec, State: accounts.JobQueued, LeaseOwner: "elsewhere", LeaseUntil: now.Add(time.Hour), CreatedAt: now.Add(-time.Minute)}
	}
	require.NoError(t, accSt.AddJob(ctx, job("job-queued")))
	failed := job("job-failed")
	failed.Repo = "secret-repo/failed"
	require.NoError(t, accSt.AddJob(ctx, failed))
	require.NoError(t, accSt.FinishJob(ctx, "job-failed", "elsewhere", accounts.JobOutcome{State: accounts.JobFailed, Error: "clone of secret-repo/failed: private-error-text", At: now}))

	// One finished ingest run with a report.
	require.NoError(t, accSt.AddRepository(ctx, accounts.Repository{OrgID: orgA.ID, Graph: "secret-repo/private", AddedBy: memberUser.ID, AddedAt: now, Visibility: accounts.Private}))
	id, err := acc.RegisterExecutionSession(ctx, memberUser.ID, orgA.ID, "jobs-section-session")
	require.NoError(t, err)
	work, err := execution.With(ctx, id)
	require.NoError(t, err)
	l, err := ingestreport.New("run-1", now)
	require.NoError(t, err)
	run := accounts.IngestRun{OrgID: orgA.ID, Graph: "secret-repo/private", Identity: id, Report: l.Snapshot()}
	require.NoError(t, accSt.BeginIngestRun(work, run))
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "skip.txt"), []byte("x"), 0600))
	require.NoError(t, l.Discover(ctx, root, nil))
	ingestreport.Skip(ingestreport.WithLedger(ctx, l), "skip.txt", ingestreport.IgnoredPath)
	ingestreport.Contribute(ingestreport.WithLedger(ctx, l), "go.mod", ingestreport.Dependencies)
	run.Report = l.Prepare(nil)
	require.NoError(t, accSt.SaveIngestRun(work, run))
	run.Report = l.Finish(ingestreport.Succeeded, ingestreport.CommitCommitted)
	require.NoError(t, accSt.SaveIngestRun(work, run))

	secrets := []string{"secret-repo", "private-error-text", "run-1", "skip.txt", "go.mod", id.SessionID, memberUser.ID, "a@example.com"}
	noSecrets := func(t *testing.T, body string) {
		t.Helper()
		for _, s := range secrets {
			assert.False(t, strings.Contains(body, s), "leaks "+s)
		}
	}

	t.Run("jobs", func(t *testing.T) {
		var body string
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/jobs?range=24h", "", &body), 200)
		noSecrets(t, body)
		var j JobsSection
		require.NoError(t, json.Unmarshal([]byte(body), &j))
		assert.True(t, j.Measured)
		assert.Equal(t, j.Totals, JobFigures{Finished: 10, OK: 6, Error: 4}, "jobs without an organisation count in the total")
		require.Len(t, j.Queue, 1)
		assert.Equal(t, j.Queue[0].Kind, "sync")
		assert.Equal(t, j.Queue[0].Active, 2-1, "the finished job left the queue")
		assert.Equal(t, j.Queue[0].Waiting, 0, "a job a worker holds does not wait")
		assert.True(t, j.Queue[0].OldestWaitingSeconds == nil)
		rows := map[string]OrgJobRow{}
		for _, r := range j.Rows {
			rows[r.OrgID] = r
		}
		assert.Equal(t, rows[orgA.ID].JobFigures, JobFigures{Finished: 5, OK: 4, Error: 1})
		assert.True(t, rows[gone].Deleted && rows[gone].Name == deletedOrg)
		assert.Equal(t, j.Rows[len(j.Rows)-1].OrgID, gone, "deleted organisations come last")
		kinds := map[string]int{}
		for _, k := range j.Kinds {
			kinds[k.Kind] = k.Finished
		}
		assert.Equal(t, kinds, map[string]int{"sync": 5, "clone": 2, "ingest": 3})

		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/jobs?range=24h&org="+orgA.ID, "", &j), 200)
		require.Len(t, j.Rows, 1)
		assert.Equal(t, j.Totals.Finished, 5)

		var sum StatsSummary
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/summary?range=24h", "", &sum), 200)
		require.True(t, sum.JobsWaiting != nil && sum.JobsFailed != nil)
		assert.Equal(t, *sum.JobsWaiting, 0)
		assert.Equal(t, *sum.JobsFailed, 4)

		for _, format := range []string{"csv", "json"} {
			require.Equal(t, ada.do("GET", "/api/v1/admin/stats/export?section=jobs&range=24h&format="+format, "", &body), 200)
			noSecrets(t, body)
		}
		assert.True(t, strings.Contains(body, `"queue"`), body)
		assert.Equal(t, member.do("GET", "/api/v1/admin/stats/jobs?range=24h", "", nil), 403)
	})

	t.Run("jobs from the rollup", func(t *testing.T) {
		day := accounts.StatDay(now).AddDate(0, 0, -1)
		set := accounts.NewPlatformStatSet(day)
		set.Both(orgA.ID, accounts.StatJobsFinished, "sync.failed", 2)
		set.Both(orgA.ID, accounts.StatJobsFinished, "sync.succeeded", 5)
		require.NoError(t, accSt.PutPlatformStats(ctx, day, set.Rows()))
		var j JobsSection
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/jobs?range=90d", "", &j), 200)
		require.True(t, j.Rollup != nil)
		assert.Equal(t, j.Rollup.Unavailable, []string{"left_behind", "cancelled"})
		assert.Equal(t, j.Totals, JobFigures{Finished: 7, OK: 5, Error: 2})
		var csv string
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/export?section=jobs&range=90d&format=csv", "", &csv), 200)
		assert.True(t, strings.HasPrefix(csv, "slug,name,deleted,finished,ok,error,left_behind,cancelled"), csv)
	})

	t.Run("ingest", func(t *testing.T) {
		var body string
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/ingest?range=24h", "", &body), 200)
		noSecrets(t, body)
		var in IngestSection
		require.NoError(t, json.Unmarshal([]byte(body), &in))
		assert.Equal(t, in.Totals.Succeeded, 1)
		assert.Equal(t, in.Totals.Scanned, 2)
		assert.Equal(t, in.Totals.Skipped, 1)
		assert.Equal(t, in.Totals.Skips.IgnoredPath, 1)
		for _, r := range in.Rows {
			if r.OrgID == orgA.ID {
				assert.Equal(t, r.Succeeded, 1)
			}
		}
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/export?section=ingest&range=24h&format=csv", "", &body), 200)
		noSecrets(t, body)
		assert.True(t, strings.HasPrefix(body, "slug,name,deleted,succeeded"), body)

		// The rollup: write today's runs as a day of their own.
		day := accounts.StatDay(now)
		rows, err := accSt.CountPlatformStats(ctx, day)
		require.NoError(t, err)
		require.NoError(t, accSt.PutPlatformStats(ctx, day.AddDate(0, 0, -2), retag(rows, day.AddDate(0, 0, -2))))
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/ingest?range=90d", "", &in), 200)
		require.True(t, in.Rollup != nil)
		assert.Equal(t, in.Totals.Succeeded, 1)
		assert.Equal(t, in.Totals.Scanned, 2)
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/ingest?range=365d&org="+orgA.ID, "", &in), 200)
		require.Len(t, in.Rows, 1)
		assert.Equal(t, in.Rows[0].Indexed, in.Totals.Indexed)
		assert.Equal(t, in.Totals.Skips.IgnoredPath, 1)
	})
}

// retag moves rows to another day.
func retag(rows []accounts.PlatformStat, day time.Time) []accounts.PlatformStat {
	for i := range rows {
		rows[i].Day = day
	}
	return rows
}

// TestAdminPanelsNoRepo keeps the repository label of the file counters out
// of every cross-tenant grouping: it may only be summed away.
func TestAdminPanelsNoRepo(t *testing.T) {
	grouping := regexp.MustCompile(`(?:by|without)\s*\(([^)]*)\)`)
	for name, p := range adminPanels {
		exprs := []string{p.groups}
		for _, q := range p.queries {
			exprs = append(exprs, q.expr)
			assert.NotEqual(t, q.by, "repo", name)
		}
		for _, e := range exprs {
			for _, m := range grouping.FindAllStringSubmatch(e, -1) {
				for _, l := range strings.Split(m[1], ",") {
					assert.NotEqual(t, strings.TrimSpace(l), "repo", name+": "+e)
				}
			}
			assert.False(t, strings.Contains(e, "repo="), name+": "+e)
		}
	}
	for _, p := range []string{"jobs_finished", "jobs_worker_roles", "ingest_files", "ingest_skips", "ingest_extract"} {
		_, ok := adminPanels[p]
		assert.True(t, ok, p)
	}
}
