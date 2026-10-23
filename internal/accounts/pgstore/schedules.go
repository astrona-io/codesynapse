package pgstore

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

const scheduleCols = "repo, org_id, actor_id, execution, interval_minutes, paused, auto_review, auto_triage, last_run, last_status, last_error, missing, next_run"

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func scheduleArgs(sc accounts.Schedule) ([]any, error) {
	exec, err := json.Marshal(sc.Execution)
	if err != nil {
		return nil, err
	}
	return []any{sc.Repo, sc.OrgID, sc.ActorID, exec, sc.IntervalMinutes, sc.Paused, sc.AutoReview, sc.AutoTriage,
		nullTime(sc.LastRun), sc.LastStatus, sc.LastError, sc.Missing, nullTime(sc.NextRun)}, nil
}

const scheduleValues = "VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)"

// ScheduleByRepo implements accounts.Store.
func (s *Store) ScheduleByRepo(ctx context.Context, repo string) (accounts.Schedule, error) {
	var sc accounts.Schedule
	var exec []byte
	var lastRun, nextRun *time.Time
	err := s.db.QueryRow(ctx, "SELECT "+scheduleCols+" FROM schedules WHERE repo = $1", repo).Scan(&sc.Repo, &sc.OrgID, &sc.ActorID, &exec,
		&sc.IntervalMinutes, &sc.Paused, &sc.AutoReview, &sc.AutoTriage, &lastRun, &sc.LastStatus, &sc.LastError, &sc.Missing, &nextRun)
	if err != nil {
		return accounts.Schedule{}, mapErr(err, "schedule")
	}
	if len(exec) > 0 {
		if err := json.Unmarshal(exec, &sc.Execution); err != nil {
			return accounts.Schedule{}, err
		}
	}
	if lastRun != nil {
		sc.LastRun = lastRun.UTC()
	}
	if nextRun != nil {
		sc.NextRun = nextRun.UTC()
	}
	return sc, nil
}

// PutSchedule implements accounts.Store.
func (s *Store) PutSchedule(ctx context.Context, sc accounts.Schedule) error {
	args, err := scheduleArgs(sc)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, "INSERT INTO schedules ("+scheduleCols+") "+scheduleValues+`
ON CONFLICT (repo) DO UPDATE SET org_id = $2, actor_id = $3, execution = $4, interval_minutes = $5, paused = $6, auto_review = $7,
  auto_triage = $8, last_run = $9, last_status = $10, last_error = $11, missing = $12, next_run = $13`, args...)
	return mapErr(err, "schedule")
}

// EnsureSchedule implements accounts.Store.
func (s *Store) EnsureSchedule(ctx context.Context, sc accounts.Schedule) error {
	args, err := scheduleArgs(sc)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, "INSERT INTO schedules ("+scheduleCols+") "+scheduleValues+" ON CONFLICT (repo) DO NOTHING", args...)
	return mapErr(err, "schedule")
}

// DeleteSchedule implements accounts.Store.
func (s *Store) DeleteSchedule(ctx context.Context, repo string) error {
	_, err := s.db.Exec(ctx, "DELETE FROM schedules WHERE repo = $1", repo)
	return err
}

// DueSchedules implements accounts.Store.
func (s *Store) DueSchedules(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := s.db.Query(ctx, `SELECT repo FROM schedules
WHERE next_run IS NOT NULL AND next_run <= $1 AND interval_minutes > 0 AND NOT paused ORDER BY repo LIMIT $2`, now, accounts.MaxSchedules)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if out == nil {
		out = []string{}
	}
	return out, err
}

// RecordScheduleRun implements accounts.Store. A removed schedule is no error.
func (s *Store) RecordScheduleRun(ctx context.Context, repo string, run accounts.ScheduleRun) error {
	_, err := s.db.Exec(ctx, `UPDATE schedules SET last_run = COALESCE($2, last_run), last_status = $3, last_error = $4,
  next_run = CASE WHEN $5::timestamptz IS NOT NULL AND interval_minutes > 0 AND NOT paused THEN $5 ELSE next_run END,
  missing = CASE WHEN $6 THEN 0 ELSE missing END
WHERE repo = $1`, repo, nullTime(run.LastRun), run.LastStatus, run.LastError, nullTime(run.NextRun), run.ResetMissing)
	return err
}

// AddScheduleMissing implements accounts.Store.
func (s *Store) AddScheduleMissing(ctx context.Context, repo string, gone bool) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, "UPDATE schedules SET missing = CASE WHEN $2 THEN missing + 1 ELSE 0 END WHERE repo = $1 RETURNING missing", repo, gone).Scan(&n)
	if err == pgx.ErrNoRows { //nolint:errorlint // pgx returns the sentinel itself
		return 0, nil
	}
	return n, err
}

// TakeLease implements accounts.Store.
func (s *Store) TakeLease(ctx context.Context, name, owner string, until, now time.Time) (bool, error) {
	tag, err := s.db.Exec(ctx, `INSERT INTO leases (name, owner, until) VALUES ($1, $2, $3)
ON CONFLICT (name) DO UPDATE SET owner = $2, until = $3 WHERE leases.owner = $2 OR leases.until < $4`, name, owner, until, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// TakeAttempt implements accounts.Store. A transaction-scoped advisory lock
// on the key makes count and insert one step across replicas.
func (s *Store) TakeAttempt(ctx context.Context, key string, limit int, since, now time.Time) (bool, error) {
	var taken bool
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 1668244584))", key); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM login_attempts WHERE at <= $1", since); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM login_attempts WHERE key = $1", key).Scan(&n); err != nil {
			return err
		}
		if n >= limit {
			return nil
		}
		taken = true
		_, err := tx.Exec(ctx, "INSERT INTO login_attempts (key, at) VALUES ($1, $2)", key, now)
		return err
	})
	return taken, err
}
