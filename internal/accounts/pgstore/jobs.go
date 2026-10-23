package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

const jobCols = "id, kind, repo, org_id, actor_id, via, key_id, role, execution, params, branch, task, state, attempts, lease_owner, lease_until, created_at, started_at, finished_at, result, error, nodes, edges"

func scanJob(row pgx.Row) (accounts.JobRecord, error) {
	var j accounts.JobRecord
	var exec []byte
	err := row.Scan(&j.ID, &j.Kind, &j.Repo, &j.OrgID, &j.ActorID, &j.Via, &j.KeyID, &j.Role, &exec, &j.Params, &j.Branch, &j.Task,
		&j.State, &j.Attempts, &j.LeaseOwner, &j.LeaseUntil, &j.CreatedAt, &j.StartedAt, &j.FinishedAt, &j.Result, &j.Error, &j.Nodes, &j.Edges)
	if err != nil {
		return j, err
	}
	if err := json.Unmarshal(exec, &j.Execution); err != nil {
		return j, fmt.Errorf("job execution: %w", err)
	}
	j.LeaseUntil, j.CreatedAt = j.LeaseUntil.UTC(), j.CreatedAt.UTC()
	for _, t := range []**time.Time{&j.StartedAt, &j.FinishedAt} {
		if *t != nil {
			u := (*t).UTC()
			*t = &u
		}
	}
	return j, nil
}

// AddJob implements accounts.Store.
func (s *Store) AddJob(ctx context.Context, j accounts.JobRecord) error {
	if err := accounts.ValidateJob(j); err != nil {
		return err
	}
	exec, err := json.Marshal(j.Execution)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO jobs (id, kind, repo, org_id, actor_id, via, key_id, role, execution, params, branch, task, state, lease_owner, lease_until, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		j.ID, j.Kind, j.Repo, j.OrgID, j.ActorID, j.Via, j.KeyID, j.Role, exec, []byte(j.Params), j.Branch, j.Task, j.State, j.LeaseOwner, j.LeaseUntil, j.CreatedAt)
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "jobs_active_repo" {
		return accounts.ErrJobActive
	}
	return mapErr(err, "job")
}

// JobByID implements accounts.Store.
func (s *Store) JobByID(ctx context.Context, id string) (accounts.JobRecord, error) {
	j, err := scanJob(s.db.QueryRow(ctx, "SELECT "+jobCols+" FROM jobs WHERE id = $1", id))
	return j, mapErr(err, "job")
}

// ListJobs implements accounts.Store. Parameters are not read.
func (s *Store) ListJobs(ctx context.Context, orgIDs []string, limit int) ([]accounts.JobRecord, error) {
	limit = min(max(limit, 1), accounts.JobHistory)
	rows, err := s.db.Query(ctx, "SELECT "+jobCols+` FROM jobs WHERE ($1::text[] IS NULL OR org_id = ANY($1))
ORDER BY created_at DESC, id LIMIT $2`, orgIDs, limit)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (accounts.JobRecord, error) {
		j, err := scanJob(r)
		j.Params = nil
		return j, err
	})
	if out == nil {
		out = []accounts.JobRecord{}
	}
	return out, err
}

// StartJob implements accounts.Store.
func (s *Store) StartJob(ctx context.Context, id, owner string, leaseUntil, now time.Time) error {
	tag, err := s.db.Exec(ctx, `UPDATE jobs SET state = 'running', attempts = attempts + 1, lease_until = $3, started_at = $4
WHERE id = $1 AND lease_owner = $2 AND state = 'queued'`, id, owner, leaseUntil, now)
	return affected(tag, err, "job")
}

// RenewJobLeases implements accounts.Store.
func (s *Store) RenewJobLeases(ctx context.Context, owner string, until time.Time) error {
	_, err := s.db.Exec(ctx, "UPDATE jobs SET lease_until = $2 WHERE lease_owner = $1 AND state IN ('queued', 'running')", owner, until)
	return err
}

// FinishJob implements accounts.Store.
func (s *Store) FinishJob(ctx context.Context, id, owner string, out accounts.JobOutcome) error {
	tag, err := s.db.Exec(ctx, `UPDATE jobs SET state = $3, result = $4, error = $5, nodes = $6, edges = $7, finished_at = $8, params = NULL, lease_owner = ''
WHERE id = $1 AND lease_owner = $2 AND state IN ('queued', 'running')`, id, owner, out.State, out.Result, out.Error, out.Nodes, out.Edges, out.At)
	if err := affected(tag, err, "job"); err != nil {
		return err
	}
	// History beyond the bound goes, oldest first; active jobs never.
	_, err = s.db.Exec(ctx, `DELETE FROM jobs WHERE id IN (SELECT id FROM jobs WHERE state IN ('succeeded', 'failed')
ORDER BY created_at DESC OFFSET $1)`, accounts.JobHistory)
	return err
}

// ClaimOrphanedJob implements accounts.Store. SKIP LOCKED lets several
// processes look at once without claiming the same job.
func (s *Store) ClaimOrphanedJob(ctx context.Context, owner string, leaseUntil, now time.Time) (accounts.JobRecord, bool, error) {
	for {
		var claimed accounts.JobRecord
		found, retry := false, false
		err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
			j, err := scanJob(tx.QueryRow(ctx, "SELECT "+jobCols+` FROM jobs WHERE state IN ('queued', 'running') AND lease_until < $1
ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED`, now))
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if j.Attempts >= accounts.MaxJobAttempts {
				retry = true
				_, err = tx.Exec(ctx, `UPDATE jobs SET state = 'failed', error = $2, finished_at = $3, params = NULL, lease_owner = '' WHERE id = $1`,
					j.ID, fmt.Sprintf("interrupted %d times; not started again", j.Attempts), now)
				return err
			}
			if _, err := tx.Exec(ctx, "UPDATE jobs SET state = 'queued', lease_owner = $2, lease_until = $3 WHERE id = $1", j.ID, owner, leaseUntil); err != nil {
				return err
			}
			j.State, j.LeaseOwner, j.LeaseUntil = accounts.JobQueued, owner, leaseUntil
			claimed, found = j, true
			return nil
		})
		if err != nil || !retry {
			return claimed, found, err
		}
	}
}
