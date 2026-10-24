package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestJobsSurviveRestart leaves jobs in the accounts store as a process that
// stopped mid-way would, then starts a server: it resumes what it may, as the
// requester and with their access checked again, and fails the rest.
func TestJobsSurviveRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	sources := t.TempDir()
	proj := filepath.Join(sources, "proj")
	require.NoError(t, os.Mkdir(proj, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(proj, "README.md"), []byte("# Proj\n"), 0o600))
	st := memory.New()
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID("proj", model.KindRepository, "proj"), Kind: model.KindRepository, Repo: "proj", Name: "proj", Root: proj})
	require.NoError(t, st.ReplaceRepo(ctx, "proj", g))

	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	u, org, err := acc.SignUp(ctx, "jobs@example.com", "Jobs", pw)
	require.NoError(t, err)
	for _, repo := range []string{"proj", "other", "gone", "tired"} {
		require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: org.ID, Graph: repo, AddedBy: u.ID, AddedAt: time.Now()}))
	}
	live, err := acc.RegisterExecutionSession(ctx, u.ID, org.ID, "thread-live")
	require.NoError(t, err)
	revoked, err := acc.RegisterExecutionSession(ctx, u.ID, org.ID, "thread-revoked")
	require.NoError(t, err)
	require.NoError(t, acc.RevokeExecutionSession(ctx, u.ID, org.ID, "thread-revoked"))

	// A process that stopped: its jobs are active with an expired lease.
	past := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	orphan := func(id, kind, repo string, id2 execution.Identity, params any, starts int) {
		raw, err := json.Marshal(params)
		require.NoError(t, err)
		// The memory store keeps Attempts as given: a job started that often.
		require.NoError(t, acc.St.AddJob(ctx, accounts.JobRecord{ID: id, Kind: kind, Repo: repo, OrgID: org.ID, ActorID: u.ID, Via: "session",
			Role: accounts.Admin, Execution: id2, Params: raw, State: accounts.JobQueued, Attempts: starts,
			LeaseOwner: "stopped-process", LeaseUntil: past, CreatedAt: past}))
	}
	orphan("sync-1", "sync", "proj", live, syncParams{}, 1)
	orphan("fix-1", "fix", "other", live, map[string]string{"review": "r1"}, 1)
	orphan("ai-1", "ai", "gone", revoked, aiTaskParams{Task: "briefing"}, 0)
	orphan("sync-2", "sync", "tired", live, syncParams{}, accounts.MaxJobAttempts)

	srv := New(ctx, Config{Accounts: acc, SourcesRoot: sources, Workspace: t.TempDir()}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	wait := func(id string) accounts.JobRecord {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			rec, err := acc.St.JobByID(ctx, id)
			require.NoError(t, err)
			if !rec.Active() || time.Now().After(deadline) {
				return rec
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	sync1 := wait("sync-1")
	assert.Equal(t, sync1.State, accounts.JobSucceeded, "the interrupted sync ran again: %s", sync1.Error)
	assert.Equal(t, sync1.Attempts, 2)
	assert.True(t, sync1.Nodes > 0, "and indexed the repository")
	assert.Equal(t, len(sync1.Params), 0)

	fix1 := wait("fix-1")
	assert.Equal(t, fix1.State, accounts.JobFailed)
	assert.True(t, strings.Contains(fix1.Error, "start the fix again"), fix1.Error)

	ai1 := wait("ai-1")
	assert.Equal(t, ai1.State, accounts.JobFailed)
	assert.True(t, strings.Contains(ai1.Error, "session ended"), "a revoked session does not run again: "+ai1.Error)

	tired := wait("sync-2")
	assert.Equal(t, tired.State, accounts.JobFailed)
	assert.True(t, strings.Contains(tired.Error, "interrupted"), tired.Error)

	// A new job is a row before it runs, with who asked and from where.
	browser := newClient(t, ts)
	require.Equal(t, browser.do("POST", "/api/v1/auth/login", `{"email":"jobs@example.com","password":"`+pw+`"}`, nil), 200)
	var started Job
	require.Equal(t, browser.do("POST", "/api/v1/repos/proj/sync", `{}`, &started, OrgHeader, org.Slug), 202)
	fresh := wait(started.ID)
	assert.Equal(t, fresh.State, accounts.JobSucceeded, fresh.Error)
	assert.Equal(t, fresh.Attempts, 1)
	assert.Equal(t, fresh.ActorID, u.ID)
	assert.Equal(t, fresh.Via, "session")
	assert.True(t, strings.HasPrefix(fresh.Execution.SessionID, "web:"), "the browser's dashboard session")

	// History survives the restart and lists through the API's view.
	job, ok := srv.jobs.Get(ctx, "sync-1")
	require.True(t, ok)
	assert.Equal(t, job.State, JobSucceeded)
	assert.Equal(t, job.Attempts, 2)
}

// TestWorkersRunWhatTheAPIRecords: the API with workers only records jobs;
// a worker claims those of its roles and runs them as the requester.
func TestWorkersRunWhatTheAPIRecords(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	sources := t.TempDir()
	proj := filepath.Join(sources, "proj")
	require.NoError(t, os.Mkdir(proj, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(proj, "README.md"), []byte("# Proj\n"), 0o600))
	st := memory.New()
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID("proj", model.KindRepository, "proj"), Kind: model.KindRepository, Repo: "proj", Name: "proj", Root: proj})
	require.NoError(t, st.ReplaceRepo(ctx, "proj", g))
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	u, org, err := acc.SignUp(ctx, "jobs@example.com", "Jobs", pw)
	require.NoError(t, err)
	require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: org.ID, Graph: "proj", AddedBy: u.ID, AddedAt: time.Now()}))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	api := New(ctx, Config{Accounts: acc, SourcesRoot: sources, Workspace: t.TempDir(), JobRoles: []string{}}, st, logger)
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)
	browser := newClient(t, ts)
	require.Equal(t, browser.do("POST", "/api/v1/auth/login", `{"email":"jobs@example.com","password":"`+pw+`"}`, nil), 200)
	var queued Job
	require.Equal(t, browser.do("POST", "/api/v1/repos/proj/sync", `{}`, &queued, OrgHeader, org.Slug), 202)
	assert.True(t, queued.Waiting, "the API only records it")
	time.Sleep(3 * jobPoll)
	rec, err := acc.St.JobByID(ctx, queued.ID)
	require.NoError(t, err)
	assert.Equal(t, rec.State, accounts.JobQueued, "nothing runs it without a worker")

	// An ai-only worker leaves it alone; an ingest worker runs it.
	_ = New(ctx, Config{Accounts: acc, SourcesRoot: sources, Workspace: t.TempDir(), JobRoles: []string{"ai"}, Worker: true}, st, logger)
	time.Sleep(3 * jobPoll)
	rec, err = acc.St.JobByID(ctx, queued.ID)
	require.NoError(t, err)
	assert.Equal(t, rec.State, accounts.JobQueued, "a worker claims only its roles")
	_ = New(ctx, Config{Accounts: acc, SourcesRoot: sources, Workspace: t.TempDir(), JobRoles: []string{"ingest"}, Worker: true}, st, logger)
	deadline := time.Now().Add(30 * time.Second)
	for rec.Active() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		rec, err = acc.St.JobByID(ctx, queued.ID)
		require.NoError(t, err)
	}
	assert.Equal(t, rec.State, accounts.JobSucceeded, rec.Error)
	assert.Equal(t, rec.Attempts, 1, "a fresh job, not a resumed one")
	assert.True(t, rec.Nodes > 0)
	var view Job
	require.Equal(t, browser.do("GET", "/api/v1/jobs/"+queued.ID, "", &view, OrgHeader, org.Slug), 200)
	assert.Equal(t, view.State, JobSucceeded, "the API shows the worker's outcome")
}

// TestJobsLeftBehind: a durable job that stops because its process stops,
// or because the process could not renew its leases, keeps its record
// active for another process to resume; any other failure is recorded.
func TestJobsLeftBehind(t *testing.T) {
	id := execution.Identity{TenantID: "org", SubjectID: "user", SessionID: "chat", ParentSessionID: "chat"}
	start := func(t *testing.T, root string) (*Jobs, *acctmem.Store, context.CancelFunc, Job, chan error) {
		t.Helper()
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		acc := acctmem.New()
		j := NewJobs(ctx, memory.New(), slog.New(slog.NewTextHandler(io.Discard, nil)), 0, false, false)
		j.UseStore(acc, []string{"sync"})
		// Hold the only slot, so the job waits holding its lease.
		j.sem <- struct{}{}
		done := make(chan error, 1)
		job, err := j.Start("proj", root, JobOptions{Identity: id, OrgID: "org", Kind: "sync",
			Durable: &durableJob{Kind: "sync", Role: accounts.Admin, ActorID: "user", Via: "session", Params: json.RawMessage(`{}`)},
			Done:    func(err error) { done <- err }})
		require.NoError(t, err)
		return j, acc, cancel, job, done
	}
	state := func(t *testing.T, acc *acctmem.Store, id string) string {
		t.Helper()
		rec, err := acc.JobByID(context.Background(), id)
		require.NoError(t, err)
		return rec.State
	}

	t.Run("process stops", func(t *testing.T) {
		_, acc, stop, job, done := start(t, t.TempDir())
		stop()
		require.Error(t, <-done)
		assert.Equal(t, state(t, acc, job.ID), accounts.JobQueued, "left for another process")
	})
	t.Run("leases lost", func(t *testing.T) {
		j, acc, _, job, done := start(t, t.TempDir())
		j.dropLeases()
		require.Error(t, <-done)
		assert.Equal(t, state(t, acc, job.ID), accounts.JobQueued, "left for another process")
		j.mu.Lock()
		assert.NoError(t, j.held.Err(), "jobs queued afterwards get fresh leases")
		j.mu.Unlock()
	})
	t.Run("other failures are recorded", func(t *testing.T) {
		j, acc, _, job, done := start(t, filepath.Join(t.TempDir(), "missing"))
		assert.False(t, j.leftBehind(t.Context(), errors.New("boom")))
		assert.False(t, j.leftBehind(t.Context(), nil))
		// The hour limit or any other cancellation is a real failure.
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		assert.False(t, j.leftBehind(ctx, ctx.Err()))
		<-j.sem
		require.Error(t, <-done, "a missing directory does not ingest")
		assert.Equal(t, state(t, acc, job.ID), accounts.JobFailed)
	})
}
