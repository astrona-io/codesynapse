package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestRetryFailedClones: a clone that failed on the network and left no graph
// is queued again once its pause is over, a bounded number of times; one that
// failed for any other reason stays failed.
func TestRetryFailedClones(t *testing.T) {
	fastRetry(t, 1)
	oldB := recloneBackoff
	recloneBackoff = func(int) time.Duration { return time.Minute }
	t.Cleanup(func() { recloneBackoff = oldB })

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	u, org, err := acc.SignUp(ctx, "reclone@example.com", "Reclone", pw)
	require.NoError(t, err)
	live, err := acc.RegisterExecutionSession(ctx, u.ID, org.ID, "thread-live")
	require.NoError(t, err)

	finished := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	failed := func(id, repo, msg string) {
		require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: org.ID, Graph: repo, Remote: "https://127.0.0.1:1/" + repo + ".git",
			Visibility: accounts.Public, AddedBy: u.ID, AddedAt: time.Now()}))
		raw, err := json.Marshal(cloneParams{Name: repo})
		require.NoError(t, err)
		require.NoError(t, acc.St.AddJob(ctx, accounts.JobRecord{ID: id, Kind: "clone", Repo: repo, OrgID: org.ID, ActorID: u.ID, Via: "session",
			Role: accounts.Admin, Execution: live, Params: raw, State: accounts.JobQueued, LeaseOwner: "test", LeaseUntil: finished, CreatedAt: finished}))
		require.NoError(t, acc.St.FinishJob(ctx, id, "test", accounts.JobOutcome{State: accounts.JobFailed, Error: msg, At: finished}))
	}
	failed("dns-1", "o/dns", "git clone: exit status 128: Could not resolve host: github.com (Timeout while contacting DNS servers)")
	failed("denied-1", "o/denied", "git clone: no access to the repository: repository not found")

	srv := New(ctx, Config{Accounts: acc, Workspace: t.TempDir()}, memory.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.retryFailedClones(ctx, time.Now(), srv.logger)

	jobs := func(repo string) (n int) {
		recs, err := acc.St.ListJobs(ctx, nil, 100)
		require.NoError(t, err)
		for _, r := range recs {
			if r.Repo == repo {
				n++
			}
		}
		return n
	}
	assert.Equal(t, jobs("o/dns"), 2, "the network failure was queued again")
	assert.Equal(t, jobs("o/denied"), 1, "a refused repository is not retried")
}
