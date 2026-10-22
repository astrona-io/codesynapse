package memstore

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/accounts"
)

// RegisterExecutionSession serializes admission and keeps closed IDs reserved.
func (s *Store) RegisterExecutionSession(ctx context.Context, session accounts.ExecutionSession) (accounts.ExecutionSession, bool, error) {
	if err := accounts.ValidateExecutionSession(session); err != nil {
		return accounts.ExecutionSession{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return accounts.ExecutionSession{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.executions[session.ID]; ok {
		if old.OrgID != session.OrgID || old.UserID != session.UserID {
			return accounts.ExecutionSession{}, false, accounts.ErrExecutionSessionClosed
		}
		if !old.Active(session.CreatedAt) && old.RevokedAt == nil {
			at := session.CreatedAt
			old.RevokedAt = &at
			s.executions[old.ID] = old
		}
		return cloneExecution(old), false, nil
	}
	active, retained := 0, 0
	for _, old := range s.executions {
		if old.OrgID == session.OrgID {
			retained++
			if old.Active(session.CreatedAt) {
				active++
			}
		}
	}
	if active >= accounts.MaxActiveExecutionSessions || retained >= accounts.MaxRetainedExecutionSessions {
		return accounts.ExecutionSession{}, false, accounts.ErrExecutionSessionLimit
	}
	s.executions[session.ID] = cloneExecution(session)
	return cloneExecution(session), true, nil
}

// ClosedExecutionSessions returns a bounded, stable page for session-memory cleanup.
func (s *Store) ClosedExecutionSessions(ctx context.Context, before time.Time, afterID string, limit int) ([]accounts.ExecutionSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []accounts.ExecutionSession{}
	for _, session := range s.executions {
		if session.ID > afterID && !session.Active(before) {
			out = append(out, cloneExecution(session))
		}
	}
	slices.SortFunc(out, func(a, b accounts.ExecutionSession) int { return cmp.Compare(a.ID, b.ID) })
	return out[:min(len(out), min(max(limit, 1), 1000))], nil
}

// ExecutionSessionByID returns an independent copy of the binding.
func (s *Store) ExecutionSessionByID(ctx context.Context, id string) (accounts.ExecutionSession, error) {
	if err := ctx.Err(); err != nil {
		return accounts.ExecutionSession{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if session, ok := s.executions[id]; ok {
		return cloneExecution(session), nil
	}
	return accounts.ExecutionSession{}, accounts.ErrNotFound
}

// RevokeExecutionSession closes only the matching tenant and participant binding.
func (s *Store) RevokeExecutionSession(ctx context.Context, id, orgID, userID string, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.executions[id]
	if !ok || session.OrgID != orgID || session.UserID != userID {
		return accounts.ErrExecutionSessionClosed
	}
	if session.RevokedAt == nil {
		session.RevokedAt = &at
		s.executions[id] = session
	}
	return nil
}

func cloneExecution(s accounts.ExecutionSession) accounts.ExecutionSession {
	if s.RevokedAt != nil {
		at := *s.RevokedAt
		s.RevokedAt = &at
	}
	return s
}
