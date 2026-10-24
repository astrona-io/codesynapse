package accountstest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// RunSchedules checks schedules and leases: settings and runs update apart,
// only active schedules come due, and a lease has one owner at a time.
func RunSchedules(t *testing.T, newStore func(*testing.T) accounts.Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	t.Run("schedules", func(t *testing.T) {
		st := newStore(t)
		_, err := st.ScheduleByRepo(ctx, "acme/api")
		assert.True(t, errors.Is(err, accounts.ErrNotFound))
		id := execution.Identity{TenantID: "org-1", SubjectID: "user-1", SessionID: "web:1", ParentSessionID: "web:1"}
		require.NoError(t, st.PutSchedule(ctx, accounts.Schedule{Repo: "acme/api", OrgID: "org-1", ActorID: "user-1", Execution: id,
			IntervalMinutes: 30, AutoReview: true, NextRun: now.Add(-time.Minute)}))
		require.NoError(t, st.EnsureSchedule(ctx, accounts.Schedule{Repo: "acme/api", IntervalMinutes: 5}))
		got, err := st.ScheduleByRepo(ctx, "acme/api")
		require.NoError(t, err)
		assert.Equal(t, got.IntervalMinutes, 30, "ensure never overwrites")
		assert.Equal(t, got.Execution, id)
		assert.True(t, got.AutoReview)
		require.NoError(t, st.EnsureSchedule(ctx, accounts.Schedule{Repo: "acme/web", IntervalMinutes: 30, NextRun: now.Add(time.Hour)}))
		require.NoError(t, st.PutSchedule(ctx, accounts.Schedule{Repo: "acme/paused", IntervalMinutes: 30, Paused: true, NextRun: now.Add(-time.Hour)}))
		due, err := st.DueSchedules(ctx, now)
		require.NoError(t, err)
		assert.Equal(t, due, []string{"acme/api"}, "only active schedules whose run has passed")

		require.NoError(t, st.RecordScheduleRun(ctx, "acme/api", accounts.ScheduleRun{LastRun: now, LastStatus: "started", NextRun: now.Add(30 * time.Minute)}))
		require.NoError(t, st.RecordScheduleRun(ctx, "acme/api", accounts.ScheduleRun{LastStatus: "succeeded", NextRun: now.Add(31 * time.Minute)}))
		got, err = st.ScheduleByRepo(ctx, "acme/api")
		require.NoError(t, err)
		assert.Equal(t, got.LastRun, now, "a run's end keeps its start")
		assert.Equal(t, got.LastStatus, "succeeded")
		assert.Equal(t, got.NextRun, now.Add(31*time.Minute))
		assert.Equal(t, got.IntervalMinutes, 30, "runs leave the settings alone")
		require.NoError(t, st.RecordScheduleRun(ctx, "acme/paused", accounts.ScheduleRun{LastStatus: "failed", NextRun: now.Add(time.Hour)}))
		paused, err := st.ScheduleByRepo(ctx, "acme/paused")
		require.NoError(t, err)
		assert.Equal(t, paused.NextRun, now.Add(-time.Hour), "a paused schedule gets no next run")

		n, err := st.AddScheduleMissing(ctx, "acme/api", true)
		require.NoError(t, err)
		assert.Equal(t, n, 1)
		n, err = st.AddScheduleMissing(ctx, "acme/api", true)
		require.NoError(t, err)
		assert.Equal(t, n, 2)
		require.NoError(t, st.RecordScheduleRun(ctx, "acme/api", accounts.ScheduleRun{LastStatus: "started", ResetMissing: true}))
		got, err = st.ScheduleByRepo(ctx, "acme/api")
		require.NoError(t, err)
		assert.Equal(t, got.Missing, 0)
		n, err = st.AddScheduleMissing(ctx, "nobody", true)
		require.NoError(t, err)
		assert.Equal(t, n, 0, "no schedule, no count")
		require.NoError(t, st.RecordScheduleRun(ctx, "nobody", accounts.ScheduleRun{LastStatus: "x"}), "a removed schedule is no error")

		require.NoError(t, st.DeleteSchedule(ctx, "acme/api"))
		_, err = st.ScheduleByRepo(ctx, "acme/api")
		assert.True(t, errors.Is(err, accounts.ErrNotFound))
	})

	t.Run("leases", func(t *testing.T) {
		st := newStore(t)
		ok, err := st.TakeLease(ctx, "loops", "a", now.Add(time.Minute), now)
		require.NoError(t, err)
		assert.True(t, ok)
		ok, err = st.TakeLease(ctx, "loops", "b", now.Add(time.Minute), now.Add(30*time.Second))
		require.NoError(t, err)
		assert.False(t, ok, "held by another")
		ok, err = st.TakeLease(ctx, "loops", "a", now.Add(2*time.Minute), now.Add(30*time.Second))
		require.NoError(t, err)
		assert.True(t, ok, "the owner renews")
		ok, err = st.TakeLease(ctx, "loops", "b", now.Add(4*time.Minute), now.Add(3*time.Minute))
		require.NoError(t, err)
		assert.True(t, ok, "an expired lease passes on")
		ok, err = st.TakeLease(ctx, "other", "a", now.Add(time.Minute), now)
		require.NoError(t, err)
		assert.True(t, ok, "leases are per name")
	})

	t.Run("attempts", func(t *testing.T) {
		st := newStore(t)
		since := now.Add(-time.Minute)
		for i := range 3 {
			ok, err := st.TakeAttempt(ctx, "k", 3, since, now.Add(time.Duration(i)*time.Second))
			require.NoError(t, err)
			assert.True(t, ok, "within the limit")
		}
		ok, err := st.TakeAttempt(ctx, "k", 3, since, now.Add(5*time.Second))
		require.NoError(t, err)
		assert.False(t, ok, "over the limit")
		ok, err = st.TakeAttempt(ctx, "other", 3, since, now.Add(5*time.Second))
		require.NoError(t, err)
		assert.True(t, ok, "limits are per key")
		later := now.Add(time.Minute + 30*time.Second)
		ok, err = st.TakeAttempt(ctx, "k", 3, later.Add(-time.Minute), later)
		require.NoError(t, err)
		assert.True(t, ok, "old attempts leave the window")
	})

	t.Run("notifications seen", func(t *testing.T) {
		st := newStore(t)
		require.NoError(t, st.CreateUser(ctx, accounts.User{ID: "u-seen", Name: "Seen", Email: "seen@example.com", CreatedAt: now}))
		at, err := st.NotificationsSeen(ctx, "u-seen")
		require.NoError(t, err)
		assert.True(t, at.IsZero(), "never marked")
		require.NoError(t, st.SetNotificationsSeen(ctx, "u-seen", now))
		require.NoError(t, st.SetNotificationsSeen(ctx, "u-seen", now.Add(-time.Hour)))
		at, err = st.NotificationsSeen(ctx, "u-seen")
		require.NoError(t, err)
		assert.True(t, at.Equal(now), "the mark only moves forward")
		assert.Error(t, st.SetNotificationsSeen(ctx, "nobody", now), "an unknown user")
	})
}
