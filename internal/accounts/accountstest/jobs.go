package accountstest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// RunJobs checks durable jobs: one active job per repository, leases that
// survive a restart only as orphans, claims that never hand one job to two
// processes, bounded retries and parameters cleared at the end.
func RunJobs(t *testing.T, newStore func(*testing.T) accounts.Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	job := func(id, repo string) accounts.JobRecord {
		return accounts.JobRecord{ID: id, Kind: "sync", Repo: repo, OrgID: "org-1", ActorID: "user-1", Via: "session", Role: accounts.Admin,
			Execution: execution.Identity{TenantID: "org-1", SubjectID: "user-1", SessionID: "web:1", ParentSessionID: "web:1"},
			Params:    json.RawMessage(`{"branch":"main"}`), Branch: "main", State: accounts.JobQueued,
			LeaseOwner: "proc-a", LeaseUntil: now.Add(time.Minute), CreatedAt: now}
	}

	t.Run("queue", func(t *testing.T) {
		st := newStore(t)
		require.NoError(t, st.AddJob(ctx, job("w1", "acme/api")))
		got, err := st.JobQueue(ctx, now)
		require.NoError(t, err)
		assert.Equal(t, len(got.Waiting), 0, "a job with a live lease is not waiting")
		assert.Equal(t, got.Active["sync"], 1, "but it is active")
		got, err = st.JobQueue(ctx, now.Add(2*time.Minute))
		require.NoError(t, err)
		assert.Equal(t, got.Waiting["sync"], 1, "an expired lease leaves the job for a worker")
		assert.True(t, got.Oldest["sync"].Equal(now), "the oldest waiting job is the queued one")
		older := job("w0", "acme/web")
		older.CreatedAt = now.Add(-time.Hour)
		require.NoError(t, st.AddJob(ctx, older))
		running := job("w2", "acme/docs")
		running.CreatedAt = now.Add(-2 * time.Hour)
		require.NoError(t, st.AddJob(ctx, running))
		require.NoError(t, st.StartJob(ctx, "w2", "proc-a", now.Add(time.Hour), now))
		got, err = st.JobQueue(ctx, now.Add(2*time.Minute))
		require.NoError(t, err)
		assert.True(t, got.Oldest["sync"].Equal(now.Add(-time.Hour)), "the oldest job that has not started, not the running one")
		assert.Equal(t, got.Active["sync"], 3)
		for _, id := range []string{"w0", "w1", "w2"} {
			require.NoError(t, st.FinishJob(ctx, id, "proc-a", accounts.JobOutcome{State: accounts.JobSucceeded, At: now}))
		}
		got, err = st.JobQueue(ctx, now.Add(2*time.Minute))
		require.NoError(t, err)
		assert.Equal(t, len(got.Waiting)+len(got.Active)+len(got.Oldest), 0, "a finished job is neither")
	})

	// A store that can wake workers does so when a job is added, so idle
	// workers need not poll every few seconds.
	t.Run("wake", func(t *testing.T) {
		st := newStore(t)
		w, ok := st.(accounts.JobWaker)
		if !ok {
			t.Skip("store cannot wake workers")
		}
		wctx, cancel := context.WithCancel(ctx)
		defer cancel()
		wake := w.JobWake(wctx)
		// A listener that just connected may wake once on its own; let it.
		select {
		case <-wake:
		case <-time.After(2 * time.Second):
		}
		require.NoError(t, st.AddJob(ctx, job("wk1", "acme/wake")))
		select {
		case <-wake:
		case <-time.After(10 * time.Second):
			t.Fatal("adding a job did not wake the worker")
		}
	})

	t.Run("lifecycle", func(t *testing.T) {
		st := newStore(t)
		require.NoError(t, st.AddJob(ctx, job("j1", "acme/api")))
		assert.True(t, errors.Is(st.AddJob(ctx, job("j2", "acme/api")), accounts.ErrJobActive), "one active job per repository")
		bad := job("j3", "acme/web")
		bad.Execution.TenantID = "org-2"
		assert.True(t, errors.Is(st.AddJob(ctx, bad), accounts.ErrInvalidInput), "the identity must match the organisation and actor")
		assert.Error(t, st.StartJob(ctx, "j1", "proc-b", now.Add(time.Minute), now), "only the owner starts it")
		require.NoError(t, st.StartJob(ctx, "j1", "proc-a", now.Add(time.Minute), now))
		got, err := st.JobByID(ctx, "j1")
		require.NoError(t, err)
		assert.Equal(t, got.State, accounts.JobRunning)
		assert.Equal(t, got.Attempts, 1)
		assert.Equal(t, string(got.Params), `{"branch":"main"}`)
		assert.Equal(t, got.Execution.SessionID, "web:1")
		require.NoError(t, st.RenewJobLeases(ctx, "proc-a", now.Add(time.Hour)))
		_, found, err := st.ClaimOrphanedJob(ctx, "proc-b", nil, now.Add(time.Minute), now.Add(30*time.Minute))
		require.NoError(t, err)
		assert.False(t, found, "a renewed lease is not an orphan")
		assert.Error(t, st.FinishJob(ctx, "j1", "proc-b", accounts.JobOutcome{State: accounts.JobSucceeded, At: now}), "only the owner finishes it")
		require.NoError(t, st.FinishJob(ctx, "j1", "proc-a", accounts.JobOutcome{State: accounts.JobSucceeded, Result: "r1", Nodes: 7, At: now}))
		got, err = st.JobByID(ctx, "j1")
		require.NoError(t, err)
		assert.Equal(t, got.State, accounts.JobSucceeded)
		assert.Equal(t, got.Nodes, 7)
		assert.Equal(t, len(got.Params), 0, "parameters are cleared when the job ends")
		require.NoError(t, st.AddJob(ctx, job("j2", "acme/api")), "the repository is free again")
		other := job("j4", "beta/api")
		other.OrgID, other.Execution.TenantID, other.CreatedAt = "org-2", "org-2", now.Add(time.Second)
		require.NoError(t, st.AddJob(ctx, other))
		list, err := st.ListJobs(ctx, []string{"org-1"}, 10)
		require.NoError(t, err)
		require.Len(t, list, 2)
		assert.Equal(t, len(list[0].Params), 0, "lists never carry parameters")
		all, err := st.ListJobs(ctx, nil, 10)
		require.NoError(t, err)
		assert.Equal(t, all[0].ID, "j4", "newest first")
	})

	t.Run("orphans after a restart", func(t *testing.T) {
		st := newStore(t)
		require.NoError(t, st.AddJob(ctx, job("j1", "acme/api")))
		require.NoError(t, st.StartJob(ctx, "j1", "proc-a", now.Add(time.Minute), now))
		later := now.Add(5 * time.Minute) // proc-a stopped renewing
		claimed, found, err := st.ClaimOrphanedJob(ctx, "proc-b", nil, later.Add(time.Minute), later)
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, claimed.ID, "j1")
		assert.Equal(t, claimed.State, accounts.JobQueued, "claimed to start again")
		assert.Equal(t, claimed.LeaseOwner, "proc-b")
		assert.Equal(t, string(claimed.Params), `{"branch":"main"}`, "with what it needs to rebuild the work")
		assert.Error(t, st.FinishJob(ctx, "j1", "proc-a", accounts.JobOutcome{State: accounts.JobSucceeded, At: later}), "the old owner lost it")
		for attempt := 2; attempt <= accounts.MaxJobAttempts; attempt++ {
			require.NoError(t, st.StartJob(ctx, "j1", "proc-b", later.Add(time.Minute), later))
			later = later.Add(5 * time.Minute)
			if attempt < accounts.MaxJobAttempts {
				_, found, err = st.ClaimOrphanedJob(ctx, "proc-b", nil, later.Add(time.Minute), later)
				require.NoError(t, err)
				require.True(t, found)
			}
		}
		_, found, err = st.ClaimOrphanedJob(ctx, "proc-c", nil, later.Add(time.Minute), later)
		require.NoError(t, err)
		assert.False(t, found, "a job interrupted too often is not started again")
		got, err := st.JobByID(ctx, "j1")
		require.NoError(t, err)
		assert.Equal(t, got.State, accounts.JobFailed)
		assert.True(t, strings.Contains(got.Error, "interrupted"), got.Error)
	})

	t.Run("queued for workers", func(t *testing.T) {
		st := newStore(t)
		queued := job("q1", "acme/api")
		queued.LeaseOwner, queued.LeaseUntil = "", time.Unix(0, 0).UTC()
		require.NoError(t, st.AddJob(ctx, queued))
		ai := job("q2", "acme/web")
		ai.Kind, ai.LeaseOwner, ai.LeaseUntil, ai.CreatedAt = "ai", "", time.Unix(0, 0).UTC(), now.Add(time.Second)
		require.NoError(t, st.AddJob(ctx, ai))
		got, found, err := st.ClaimOrphanedJob(ctx, "worker-ai", []string{"ai"}, now.Add(time.Minute), now)
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, got.ID, "q2", "a worker claims only its kinds")
		assert.Equal(t, got.Attempts, 0, "never started yet")
		_, found, err = st.ClaimOrphanedJob(ctx, "worker-ai", []string{"ai"}, now.Add(time.Minute), now)
		require.NoError(t, err)
		assert.False(t, found)
		got, found, err = st.ClaimOrphanedJob(ctx, "worker-ingest", []string{"sync", "clone"}, now.Add(time.Minute), now)
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, got.ID, "q1")
		require.NoError(t, st.StartJob(ctx, "q1", "worker-ingest", now.Add(time.Minute), now))
	})

	t.Run("concurrent claims", func(t *testing.T) {
		st := newStore(t)
		for i, repo := range []string{"a", "b", "c", "d"} {
			j := job("j"+repo, repo)
			j.CreatedAt = now.Add(time.Duration(i) * time.Second)
			require.NoError(t, st.AddJob(ctx, j))
		}
		later := now.Add(time.Hour)
		var mu sync.Mutex
		seen := map[string]int{}
		var wg sync.WaitGroup
		for w := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					j, found, err := st.ClaimOrphanedJob(ctx, fmt.Sprintf("proc-%d", w), nil, later.Add(time.Hour), later)
					if err != nil || !found {
						return
					}
					mu.Lock()
					seen[j.ID]++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		assert.Equal(t, len(seen), 4)
		for id, n := range seen {
			assert.Equal(t, n, 1, id+" claimed once")
		}
	})
}
