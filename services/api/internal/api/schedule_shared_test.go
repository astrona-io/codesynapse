package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestSchedulesAreShared: with accounts, schedules live in the accounts
// store, so an API and a worker see the same ones, and a schedules file from
// before moves in once.
func TestSchedulesAreShared(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	workspace := t.TempDir()
	file := filepath.Join(workspace, ".codesynapse", "schedules.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o700))
	old, err := json.Marshal([]Schedule{{Repo: "acme/api", OrgID: "org-1", IntervalMinutes: 60, AutoTriage: true, NextRun: "2026-10-23T10:00:00Z"}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, old, 0o600))

	acc := accounts.NewService(acctmem.New(), true)
	api := New(ctx, Config{Accounts: acc, Workspace: workspace, JobRoles: []string{}}, memory.New(), logger)
	got := api.sched.get(ctx, "acme/api")
	assert.Equal(t, got.IntervalMinutes, 60, "the file moved into the database")
	assert.True(t, got.AutoTriage)
	assert.Equal(t, got.NextRun, "2026-10-23T10:00:00Z")
	_, err = os.Stat(file)
	assert.True(t, os.IsNotExist(err), "and is not read again")
	_, err = os.Stat(file + ".imported")
	require.NoError(t, err)

	worker := New(ctx, Config{Accounts: acc, Workspace: t.TempDir(), JobRoles: []string{"ingest"}, Worker: true}, memory.New(), logger)
	assert.Equal(t, worker.sched.get(ctx, "acme/api").IntervalMinutes, 60, "a worker sees the same schedules")
	now := time.Now().UTC().Truncate(time.Second)
	worker.sched.finished(ctx, "acme/api", now, nil)
	got = api.sched.get(ctx, "acme/api")
	assert.Equal(t, got.LastStatus, "succeeded", "and its report reaches the API")
	assert.Equal(t, got.NextRun, now.Add(time.Hour).Format(time.RFC3339))
	assert.True(t, got.AutoTriage, "settings stay")
	assert.Equal(t, api.sched.due(ctx, now.Add(2*time.Hour)), []string{"acme/api"})
}

// TestOneAPIRunsTheLoops: of two API processes, only the one holding the
// lease runs the singleton loops.
func TestOneAPIRunsTheLoops(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	acc := accounts.NewService(acctmem.New(), true)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var running atomic.Int32
	loop := func(ctx context.Context) {
		running.Add(1)
		<-ctx.Done()
		running.Add(-1)
	}
	for range 2 {
		s := &Server{cfg: Config{Accounts: acc}, logger: logger}
		s.singleton(ctx, loop)
	}
	deadline := time.Now().Add(5 * time.Second)
	for running.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, running.Load(), int32(1), "exactly one process leads")
}
