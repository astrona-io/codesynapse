package pgstore

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// RegisterExecutionSession serializes admission across replicas and retains IDs.
func (s *Store) RegisterExecutionSession(ctx context.Context, session accounts.ExecutionSession) (accounts.ExecutionSession, bool, error) {
	if err := accounts.ValidateExecutionSession(session); err != nil {
		return accounts.ExecutionSession{}, false, err
	}
	var out accounts.ExecutionSession
	created := false
	err := s.PolicyTransaction(ctx, session.OrgID, func(st accounts.Store) error {
		tx := st.(*Store)
		old, err := tx.ExecutionSessionByID(ctx, session.ID)
		if err == nil {
			if old.OrgID != session.OrgID || old.UserID != session.UserID {
				return accounts.ErrExecutionSessionClosed
			}
			if !old.Active(session.CreatedAt) && old.RevokedAt == nil {
				if err := tx.RevokeExecutionSession(ctx, old.ID, old.OrgID, old.UserID, session.CreatedAt); err != nil {
					return err
				}
				at := session.CreatedAt
				old.RevokedAt = &at
			}
			out = old
			return nil
		}
		if !errors.Is(err, accounts.ErrNotFound) {
			return err
		}
		var active, retained int
		if err := tx.db.QueryRow(ctx, `SELECT count(*) FILTER (WHERE revoked_at IS NULL AND expires_at > $2), count(*) FROM execution_sessions WHERE org_id=$1`, session.OrgID, session.CreatedAt).Scan(&active, &retained); err != nil {
			return err
		}
		if active >= accounts.MaxActiveExecutionSessions || retained >= accounts.MaxRetainedExecutionSessions {
			return accounts.ErrExecutionSessionLimit
		}
		_, err = tx.db.Exec(ctx, `INSERT INTO execution_sessions (id,user_id,org_id,created_at,expires_at,revoked_at) VALUES ($1,$2,$3,$4,$5,$6)`, session.ID, session.UserID, session.OrgID, session.CreatedAt, session.ExpiresAt, session.RevokedAt)
		if err != nil {
			return err
		}
		out, created = session, true
		return nil
	})
	return out, created, err
}

// ClosedExecutionSessions returns a bounded, stable page for session-memory cleanup.
func (s *Store) ClosedExecutionSessions(ctx context.Context, before time.Time, afterID string, limit int) ([]accounts.ExecutionSession, error) {
	rows, err := s.db.Query(ctx, `SELECT id,user_id,org_id,created_at,expires_at,revoked_at FROM execution_sessions WHERE id > $1 AND (revoked_at IS NOT NULL OR expires_at <= $2) ORDER BY id LIMIT $3`, afterID, before, min(max(limit, 1), 1000))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (accounts.ExecutionSession, error) {
		var session accounts.ExecutionSession
		err := r.Scan(&session.ID, &session.UserID, &session.OrgID, &session.CreatedAt, &session.ExpiresAt, &session.RevokedAt)
		session.CreatedAt, session.ExpiresAt = session.CreatedAt.UTC(), session.ExpiresAt.UTC()
		return session, err
	})
}

// ExecutionSessionByID reads the permanent runtime binding.
func (s *Store) ExecutionSessionByID(ctx context.Context, id string) (accounts.ExecutionSession, error) {
	var session accounts.ExecutionSession
	err := s.db.QueryRow(ctx, `SELECT id,user_id,org_id,created_at,expires_at,revoked_at FROM execution_sessions WHERE id=$1`, id).Scan(&session.ID, &session.UserID, &session.OrgID, &session.CreatedAt, &session.ExpiresAt, &session.RevokedAt)
	session.CreatedAt, session.ExpiresAt = session.CreatedAt.UTC(), session.ExpiresAt.UTC()
	return session, mapErr(err, "runtime session")
}

// RevokeExecutionSession cannot change a binding or resurrect a closed ID.
func (s *Store) RevokeExecutionSession(ctx context.Context, id, orgID, userID string, at time.Time) error {
	tag, err := s.db.Exec(ctx, `UPDATE execution_sessions SET revoked_at=COALESCE(revoked_at,$4) WHERE id=$1 AND org_id=$2 AND user_id=$3`, id, orgID, userID, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return accounts.ErrExecutionSessionClosed
	}
	return nil
}
