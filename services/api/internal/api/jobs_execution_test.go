package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func fixedExecution() execution.Identity {
	return execution.Identity{TenantID: "test-org", SubjectID: "test-user", SessionID: "runtime-thread", ParentSessionID: "runtime-thread"}
}

func TestJobExecutionInheritanceAndMissingContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	jobs := NewJobs(ctx, memory.New(), slog.New(slog.NewTextHandler(io.Discard, nil)), 0, false, false)
	_, err := jobs.Start("missing", "", JobOptions{})
	require.Error(t, err)
	assert.Len(t, jobs.List(), 0, "missing context is rejected before queue insertion")
	id := fixedExecution()
	done := make(chan error, 1)
	job, err := jobs.Start("repo", "", JobOptions{Identity: id, OrgID: id.TenantID,
		Run: func(ctx context.Context) (string, error) {
			got, err := execution.Require(ctx)
			assert.NoError(t, err)
			assert.Equal(t, got, id)
			return "result", nil
		}, Done: func(err error) { done <- err }})
	require.NoError(t, err)
	assert.Equal(t, job.Identity, id)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("job did not finish")
	}
}

func TestQueuedJobRechecksRuntimeRevocationAndCancellation(t *testing.T) {
	for _, mode := range []string{"revoke", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			jobs := NewJobs(ctx, memory.New(), slog.New(slog.NewTextHandler(io.Discard, nil)), 0, false, false)
			jobs.sem <- struct{}{}
			var revoked atomic.Bool
			var ran atomic.Bool
			done := make(chan error, 1)
			_, err := jobs.Start("repo", "", JobOptions{Identity: fixedExecution(),
				Authorize: func(context.Context) error {
					if revoked.Load() {
						return errors.New("runtime session revoked")
					}
					return nil
				}, Run: func(context.Context) (string, error) { ran.Store(true); return "", nil },
				Done: func(err error) { done <- err }})
			require.NoError(t, err)
			if mode == "cancel" {
				cancel()
			} else {
				revoked.Store(true)
			}
			<-jobs.sem
			select {
			case err := <-done:
				require.Error(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("queued job did not finish")
			}
			assert.False(t, ran.Load())
		})
	}
}

func TestSchedulesPersistRuntimeIdentity(t *testing.T) {
	dir := t.TempDir()
	schedules, err := loadSchedules(dir)
	require.NoError(t, err)
	id := fixedExecution()
	_, err = schedules.set("repo", 15, false, true, false, time.Now(), Schedule{OrgID: id.TenantID, ActorID: id.SubjectID, Execution: id})
	require.NoError(t, err)
	reloaded, err := loadSchedules(dir)
	require.NoError(t, err)
	assert.Equal(t, reloaded.get("repo").Execution, id)
	assert.True(t, execution.Validate(reloaded.get("legacy-missing").Execution) != nil, "legacy schedules never get fabricated runtime identities")
}

func TestAccountslessAutomationRejectsPersistedIdentity(t *testing.T) {
	ctx, err := execution.With(t.Context(), fixedExecution())
	require.NoError(t, err)
	s := &Server{}
	_, authorize := s.jobAuthorization(ctx, "repo", accounts.Admin)
	assert.True(t, errors.Is(authorize(ctx), errExecutionRegistry))
	_, err = s.scheduleContext(t.Context(), Schedule{Repo: "repo", OrgID: "test-org", ActorID: "test-user", Execution: fixedExecution()})
	assert.True(t, errors.Is(err, errExecutionRegistry))
	s.orgs, err = loadOrgs("")
	require.NoError(t, err)
	o := Org{Org: "acme", Host: "github.com", Execution: fixedExecution()}
	require.NoError(t, s.orgs.put(o))
	s.scanDue(t.Context(), time.Now(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	got, ok := s.orgs.get(o.ident())
	require.True(t, ok)
	assert.Equal(t, got.LastError, errExecutionRegistry.Error())
	assert.NotEqual(t, got.NextScan, "", "unvalidated scans back off without contacting a forge")
}

func TestScheduleRequiresLiveRuntimeDelegation(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	f.client.auth = ""
	require.Equal(t, f.client.do("POST", "/api/v1/auth/login", `{"email":"runtime@example.com","password":"`+pw+`"}`, nil), 200)
	path := "/api/v1/repos/" + storetest.Repo + "/schedule"
	require.Equal(t, f.client.do("PUT", path, `{"interval_minutes":60}`, nil, runtimeHeaders("thread-a")...), 200)
	sc := f.server.sched.get(storetest.Repo)
	assert.Equal(t, sc.Execution, f.identity)
	ctx, err := f.server.scheduleContext(t.Context(), sc)
	require.NoError(t, err)
	id, err := execution.Require(ctx)
	require.NoError(t, err)
	assert.Equal(t, id, f.identity)
	require.NoError(t, f.accounts.RevokeExecutionSession(t.Context(), f.user.ID, f.org.ID, "thread-a"))
	_, err = f.server.scheduleContext(t.Context(), sc)
	assert.True(t, errors.Is(err, accounts.ErrExecutionSessionClosed))
}

func TestAttachmentRequiresRuntimeBeforeGraphLookup(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	f.client.auth = ""
	require.Equal(t, f.client.do("POST", "/api/v1/auth/login", `{"email":"runtime@example.com","password":"`+pw+`"}`, nil), 200)
	path := "/api/v1/accounts/orgs/" + f.org.Slug + "/repos"
	assert.Equal(t, f.client.do("POST", path, `{"graph":"missing"}`, nil), 403)
	assert.Equal(t, f.client.do("POST", path, `{"graph":"missing"}`, nil, runtimeHeaders("thread-a")...), 404)
}

func TestRetentionPersistsObservedExpiry(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	created := time.Now().Add(-accounts.ExecutionSessionTTL - time.Hour)
	record := accounts.ExecutionSession{ID: "expired-retention", UserID: f.user.ID, OrgID: f.org.ID, CreatedAt: created, ExpiresAt: created.Add(accounts.ExecutionSessionTTL)}
	_, _, err := f.accounts.St.RegisterExecutionSession(t.Context(), record)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go f.server.pruneExecutionMemory(ctx)
	for range 100 {
		got, err := f.accounts.St.ExecutionSessionByID(t.Context(), record.ID)
		require.NoError(t, err)
		if got.RevokedAt != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("cleanup did not persist the observed execution expiry")
}
