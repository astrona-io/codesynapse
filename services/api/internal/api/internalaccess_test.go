package api

import (
	"context"
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
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestCallerAccess: the API answers for the caller only, with their own key
// and a runtime session of theirs in the organisation asked about.
func TestCallerAccess(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	session := runtimeHeaders(f.identity.SessionID)
	body := `{"org_id":"` + f.org.ID + `"}`

	var a accounts.Access
	require.Equal(t, f.client.do("POST", callerAccessPath[5:], body, &a, session...), 200)
	assert.Equal(t, a.UserID, f.user.ID)
	require.Equal(t, len(a.Repos), 1)
	assert.Equal(t, a.Repos[0].Graph, storetest.Repo)
	assert.True(t, a.OrgPerms[f.org.ID] != 0, "the organisation's permissions come along")

	var narrowed accounts.Access
	require.Equal(t, f.client.do("POST", callerAccessPath[5:], `{"org_id":"`+f.org.ID+`","graphs":["elsewhere"]}`, &narrowed, session...), 200)
	assert.Equal(t, len(narrowed.Repos), 0, "only the graphs asked about")

	var orgOnly accounts.Access
	require.Equal(t, f.client.do("POST", callerAccessPath[5:], `{"org_id":"`+f.org.ID+`","org_only":true}`, &orgOnly, session...), 200)
	assert.Equal(t, len(orgOnly.Repos), 0)
	assert.Equal(t, orgOnly.Orgs[f.org.ID], a.Orgs[f.org.ID])

	assert.Equal(t, f.client.do("POST", callerAccessPath[5:], body, nil), 403, "no runtime session")
	assert.Equal(t, f.client.do("POST", callerAccessPath[5:], `{"org_id":"another"}`, nil, session...), 403, "another organisation")
	assert.Equal(t, f.client.do("POST", callerAccessPath[5:], `{}`, nil, session...), 400)
	many := `{"org_id":"` + f.org.ID + `","graphs":["` + strings.Repeat(`g","`, accounts.MaxAccessGraphs) + `g"]}`
	assert.Equal(t, f.client.do("POST", callerAccessPath[5:], many, nil, session...), 400, "bounded batch")

	anonymous := newClient(t, f.client.ts)
	assert.Equal(t, anonymous.do("POST", callerAccessPath[5:], body, nil, session...), 401)
	// A job lease is no credential here.
	assert.Equal(t, anonymous.do("POST", callerAccessPath[5:], body, nil, JobLeaseHeader, "anything"), 401)
}

// TestJobAccess: a worker proves the job's lease and gets the requester's
// access to that job's repository, rebuilt from the record; nothing else.
func TestJobAccess(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	ctx := t.Context()
	owner := newOwner()
	rec := accounts.JobRecord{ID: "job-access", Kind: "sync", Repo: storetest.Repo, OrgID: f.org.ID, ActorID: f.user.ID, Via: "session",
		Role: accounts.Read, Execution: f.identity, State: accounts.JobQueued, LeaseOwner: owner,
		LeaseUntil: time.Now().Add(time.Hour).UTC(), CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	require.NoError(t, f.accounts.St.AddJob(ctx, rec))
	require.NoError(t, f.accounts.St.StartJob(ctx, rec.ID, owner, time.Now().Add(time.Hour).UTC(), time.Now().UTC()))

	worker := jobAccessHTTP(f.client.ts.URL, owner)
	p := accounts.Principal{User: accounts.User{ID: f.user.ID}, Via: "session"}
	jobCtx := withJobID(ctx, rec.ID)
	a, err := worker.AccessIn(jobCtx, p, accounts.AccessQuery{OrgID: f.org.ID})
	require.NoError(t, err)
	assert.Equal(t, a.UserID, f.user.ID)
	require.Equal(t, len(a.Repos), 1, "the job's repository only")
	assert.Equal(t, a.Repos[0].Graph, storetest.Repo)

	// A worker's accounts service decides through the API.
	remote := accounts.NewService(f.accounts.St, true)
	remote.Remote = worker
	got, err := remote.ResolveRepository(jobCtx, p, f.org.ID, storetest.Repo, accounts.Read)
	require.NoError(t, err)
	assert.Equal(t, got.Graph, storetest.Repo)

	refused := func(t *testing.T, c *jobAccessClient, ctx context.Context, p accounts.Principal, q accounts.AccessQuery) {
		t.Helper()
		_, err := c.AccessIn(ctx, p, q)
		assert.True(t, errors.Is(err, accounts.ErrForbidden) || errors.Is(err, accounts.ErrNotFound) || errors.Is(err, accounts.ErrInvalidInput), "refused: %v", err)
	}
	t.Run("no job in the context", func(t *testing.T) {
		refused(t, worker, ctx, p, accounts.AccessQuery{OrgID: f.org.ID})
		_, err := remote.ResolveRepository(ctx, p, f.org.ID, storetest.Repo, accounts.Read)
		assert.True(t, err != nil)
	})
	t.Run("wrong lease", func(t *testing.T) {
		refused(t, jobAccessHTTP(f.client.ts.URL, newOwner()), jobCtx, p, accounts.AccessQuery{OrgID: f.org.ID})
	})
	t.Run("someone else", func(t *testing.T) {
		refused(t, worker, jobCtx, accounts.Principal{User: accounts.User{ID: "other"}, Via: "session"}, accounts.AccessQuery{OrgID: f.org.ID})
		refused(t, worker, jobCtx, accounts.Principal{User: p.User, Via: "key", KeyID: "k"}, accounts.AccessQuery{OrgID: f.org.ID})
	})
	t.Run("another repository or organisation", func(t *testing.T) {
		refused(t, worker, jobCtx, p, accounts.AccessQuery{OrgID: f.org.ID, Graphs: []string{"elsewhere"}})
		refused(t, worker, jobCtx, p, accounts.AccessQuery{OrgID: "another"})
	})
	t.Run("signed-in callers do not use this route", func(t *testing.T) {
		status := f.client.do("POST", "/api/v1/internal/jobs/"+rec.ID+"/access", `{"user_id":"`+f.user.ID+`","via":"session","org_id":"`+f.org.ID+`"}`, nil, JobLeaseHeader, owner)
		assert.Equal(t, status, 403)
	})
	t.Run("a finished job", func(t *testing.T) {
		require.NoError(t, f.accounts.St.FinishJob(ctx, rec.ID, owner, accounts.JobOutcome{State: accounts.JobSucceeded, At: time.Now().UTC()}))
		refused(t, worker, jobCtx, p, accounts.AccessQuery{OrgID: f.org.ID})
	})
	t.Run("the requester lost the session", func(t *testing.T) {
		other := rec
		other.ID, other.Repo = "job-revoked", "revoked-repo"
		require.NoError(t, f.accounts.St.AddRepository(ctx, accounts.Repository{OrgID: f.org.ID, Graph: other.Repo, AddedBy: f.user.ID, AddedAt: time.Now()}))
		require.NoError(t, f.accounts.St.AddJob(ctx, other))
		otherCtx := withJobID(ctx, other.ID)
		_, err := worker.AccessIn(otherCtx, p, accounts.AccessQuery{OrgID: f.org.ID})
		require.NoError(t, err, "answered while the session lives")
		require.NoError(t, f.accounts.RevokeExecutionSession(ctx, f.user.ID, f.org.ID, f.identity.SessionID))
		refused(t, worker, otherCtx, p, accounts.AccessQuery{OrgID: f.org.ID})
	})
}

func TestLeaseHeld(t *testing.T) {
	now := time.Now()
	rec := accounts.JobRecord{State: accounts.JobRunning, LeaseOwner: "0123456789abcdef", LeaseUntil: now.Add(time.Minute)}
	assert.True(t, leaseHeld(rec, "0123456789abcdef", now))
	for name, tc := range map[string]struct {
		rec   accounts.JobRecord
		owner string
	}{
		"another owner":   {rec, "fedcba9876543210"},
		"a prefix":        {rec, "0123"},
		"no owner":        {accounts.JobRecord{State: accounts.JobRunning, LeaseUntil: now.Add(time.Minute)}, ""},
		"expired":         {accounts.JobRecord{State: accounts.JobRunning, LeaseOwner: rec.LeaseOwner, LeaseUntil: now.Add(-time.Second)}, rec.LeaseOwner},
		"finished":        {accounts.JobRecord{State: accounts.JobSucceeded, LeaseOwner: rec.LeaseOwner, LeaseUntil: now.Add(time.Minute)}, rec.LeaseOwner},
		"empty presented": {rec, ""},
	} {
		t.Run(name, func(t *testing.T) { assert.False(t, leaseHeld(tc.rec, tc.owner, now)) })
	}
}

// TestWorkerAsksTheAPI: a worker with an API URL decides nothing itself; the
// job runs with every check answered by the API, which audits each question.
func TestWorkerAsksTheAPI(t *testing.T) {
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
	accStore := acctmem.New()
	acc := accounts.NewService(accStore, true)
	bootstrapAccounts(t, acc)
	u, org, err := acc.SignUp(ctx, "jobs@example.com", "Jobs", pw)
	require.NoError(t, err)
	require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: org.ID, Graph: "proj", AddedBy: u.ID, AddedAt: time.Now()}))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ts := httptest.NewServer(New(ctx, Config{Accounts: acc, SourcesRoot: sources, Workspace: t.TempDir(), JobRoles: []string{}}, st, logger))
	t.Cleanup(ts.Close)
	browser := newClient(t, ts)
	require.Equal(t, browser.do("POST", "/api/v1/auth/login", `{"email":"jobs@example.com","password":"`+pw+`"}`, nil), 200)
	var queued Job
	require.Equal(t, browser.do("POST", "/api/v1/repos/proj/sync", `{}`, &queued, OrgHeader, org.Slug), 202)

	// The worker's own accounts service, as in its own process.
	workerAcc := accounts.NewService(accStore, true)
	_ = New(ctx, Config{Accounts: workerAcc, SourcesRoot: sources, Workspace: t.TempDir(), JobRoles: []string{"ingest"}, Worker: true, AccessURL: ts.URL}, st, logger)
	require.True(t, workerAcc.Remote != nil, "the worker asks the API")
	rec, err := acc.St.JobByID(ctx, queued.ID)
	require.NoError(t, err)
	deadline := time.Now().Add(30 * time.Second)
	for rec.Active() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		rec, err = acc.St.JobByID(ctx, queued.ID)
		require.NoError(t, err)
	}
	assert.Equal(t, rec.State, accounts.JobSucceeded, rec.Error)
	evs, err := acc.St.ListEvents(ctx, accounts.EventFilter{Action: "http " + jobAccessPath, Limit: 100})
	require.NoError(t, err)
	assert.True(t, len(evs) > 0, "every question is audited")
	for _, e := range evs {
		assert.Equal(t, e.Outcome, accounts.Success)
	}
}
