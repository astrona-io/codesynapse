package api

import (
	"context"
	"encoding/json"
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
