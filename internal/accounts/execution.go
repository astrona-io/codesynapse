package accounts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/astrona-io/codesynapse/internal/execution"
)

// Execution sessions are distinct from browser login sessions. Expiry never slides.
const (
	ExecutionSessionTTL          = 7 * 24 * time.Hour
	MaxActiveExecutionSessions   = 1024
	MaxRetainedExecutionSessions = 65536
)

// Execution-session errors expose no rejected identifiers or participant details.
var (
	ErrExecutionSessionClosed = errors.New("runtime session is unavailable, expired or revoked")
	ErrExecutionSessionLimit  = errors.New("organisation runtime session limit reached")
)

// ExecutionSession binds one upstream session to its sole approved participant.
// Closed rows are permanent tombstones; deleting them would permit replay.
type ExecutionSession struct {
	ID        string     `json:"session_id"`
	UserID    string     `json:"subject_id"`
	OrgID     string     `json:"tenant_id"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// Active reports whether execution can continue at now.
func (s ExecutionSession) Active(now time.Time) bool {
	return s.RevokedAt == nil && now.Before(s.ExpiresAt)
}

// ValidateExecutionSession checks a new binding before either store admits it.
func ValidateExecutionSession(s ExecutionSession) error {
	if err := execution.Validate(execution.Identity{TenantID: s.OrgID, SubjectID: s.UserID, SessionID: s.ID, ParentSessionID: s.ID}); err != nil {
		return ErrInvalidInput
	}
	if s.CreatedAt.IsZero() || !s.ExpiresAt.Equal(s.CreatedAt.Add(ExecutionSessionTTL)) || s.RevokedAt != nil {
		return ErrInvalidInput
	}
	return nil
}

func executionIdentity(ctx context.Context, userID, orgID, sessionID string) (execution.Identity, error) {
	id := execution.Identity{TenantID: orgID, SubjectID: userID, SessionID: sessionID, ParentSessionID: sessionID}
	if _, err := execution.With(ctx, id); err != nil {
		return execution.Identity{}, err
	}
	return id, nil
}

func executionMember(ctx context.Context, st Store, userID, orgID string) error {
	u, err := st.UserByID(ctx, userID)
	if err != nil || u.DisabledAt != nil {
		return ErrForbidden
	}
	if err := businessAllowed(ctx, st, u); err != nil {
		return err
	}
	role, err := st.MemberRole(ctx, orgID, userID)
	if err != nil || !role.Valid() {
		return ErrForbidden
	}
	return nil
}

// RegisterExecutionSession binds the runtime-provided ID without inventing or
// extending it. Ingress must first authenticate the caller and selected tenant.
func (s *Service) RegisterExecutionSession(ctx context.Context, userID, orgID, sessionID string) (execution.Identity, error) {
	id, err := executionIdentity(ctx, userID, orgID, sessionID)
	if err != nil {
		return execution.Identity{}, err
	}
	now := s.now()
	record, err := policyChange(ctx, s, func(tx *Service) (ExecutionSession, error) {
		if err := executionMember(ctx, tx.St, userID, orgID); err != nil {
			return ExecutionSession{}, err
		}
		record, created, err := tx.St.RegisterExecutionSession(ctx, ExecutionSession{ID: sessionID, UserID: userID, OrgID: orgID, CreatedAt: now, ExpiresAt: now.Add(ExecutionSessionTTL)})
		if err != nil || !created {
			return record, err
		}
		_, err = tx.Record(ctx, Event{At: now, UserID: userID, OrgID: orgID, Action: "execution_session.register", Target: sessionID, Outcome: Success})
		return record, err
	})
	if err != nil {
		return execution.Identity{}, err
	}
	if !record.Active(now) {
		return execution.Identity{}, ErrExecutionSessionClosed
	}
	return id, nil
}

// ResolveExecutionSession rechecks membership, suspension and the session binding.
// A known session does not authorize a different user, tenant or repository.
func (s *Service) ResolveExecutionSession(ctx context.Context, userID, orgID, sessionID string) (execution.Identity, error) {
	id, err := executionIdentity(ctx, userID, orgID, sessionID)
	if err != nil {
		return execution.Identity{}, err
	}
	var record ExecutionSession
	err = s.St.ReadSnapshot(ctx, func(st Store) error {
		if err := executionMember(ctx, st, userID, orgID); err != nil {
			return err
		}
		var err error
		record, err = st.ExecutionSessionByID(ctx, sessionID)
		if err != nil || record.OrgID != orgID || record.UserID != userID {
			return ErrExecutionSessionClosed
		}
		return nil
	})
	if err != nil {
		return execution.Identity{}, err
	}
	if !record.Active(s.now()) {
		// Persist observed expiry so clock rollback cannot revive the session.
		if record.RevokedAt == nil {
			if err := s.St.RevokeExecutionSession(ctx, sessionID, orgID, userID, s.now()); err != nil {
				return execution.Identity{}, err
			}
		}
		return execution.Identity{}, ErrExecutionSessionClosed
	}
	return id, nil
}

// RevokeExecutionSession closes this participant's session permanently.
func (s *Service) RevokeExecutionSession(ctx context.Context, userID, orgID, sessionID string) error {
	if _, err := executionIdentity(ctx, userID, orgID, sessionID); err != nil {
		return err
	}
	_, err := policyChange(ctx, s, func(tx *Service) (struct{}, error) {
		if err := executionMember(ctx, tx.St, userID, orgID); err != nil {
			return struct{}{}, err
		}
		record, err := tx.St.ExecutionSessionByID(ctx, sessionID)
		if err != nil || record.UserID != userID || record.OrgID != orgID {
			return struct{}{}, ErrExecutionSessionClosed
		}
		if record.RevokedAt != nil {
			return struct{}{}, nil
		}
		if err := tx.St.RevokeExecutionSession(ctx, sessionID, orgID, userID, s.now()); err != nil {
			return struct{}{}, err
		}
		_, err = tx.Record(ctx, Event{At: s.now(), UserID: userID, OrgID: orgID, Action: "execution_session.revoke", Target: sessionID, Outcome: Success})
		return struct{}{}, err
	})
	return err
}

// DashboardExecutionSession is the runtime binding of one signed-in browser in
// one organisation. The dashboard has no chat thread to name, so the ID comes
// from the login session's hash, the organisation and a window as long as
// ExecutionSessionTTL: only that browser session reaches it, a new sign-in
// gets a new one, and the binding a window opens is still active when the
// next window starts, so there is no gap. API keys and the service token never
// get one; their runtimes register their own IDs.
func (s *Service) DashboardExecutionSession(ctx context.Context, p Principal, orgID string) (execution.Identity, error) {
	if p.Via != "session" || p.SessionHash == "" {
		return execution.Identity{}, ErrForbidden
	}
	window := s.now().Unix() / int64(ExecutionSessionTTL/time.Second)
	sum := sha256.Sum256([]byte("dashboard|" + p.SessionHash + "|" + orgID + "|" + strconv.FormatInt(window, 10)))
	id := "web:" + hex.EncodeToString(sum[:20])
	if got, err := s.ResolveExecutionSession(ctx, p.User.ID, orgID, id); err == nil {
		return got, nil
	}
	return s.RegisterExecutionSession(ctx, p.User.ID, orgID, id)
}

// GatewayExecutionSession is the runtime binding of one gateway key. The agent
// that sends model traffic through the gateway has no chat ID to name, so the
// ID comes from the key's own ID, the organisation and a window as long as
// ExecutionSessionTTL: only that key reaches it, a new key gets a new one, and
// the next window opens before the last closes. Only keys get one; browser
// sessions use DashboardExecutionSession and the service token gets none. The
// key itself is rechecked by AuthenticateKey on every request, so revoking it
// ends the traffic at once.
func (s *Service) GatewayExecutionSession(ctx context.Context, p Principal, orgID string) (execution.Identity, error) {
	if p.Via != "key" || p.KeyID == "" || p.OrgID != orgID {
		return execution.Identity{}, ErrForbidden
	}
	window := s.now().Unix() / int64(ExecutionSessionTTL/time.Second)
	sum := sha256.Sum256([]byte("gateway|" + p.KeyID + "|" + orgID + "|" + strconv.FormatInt(window, 10)))
	id := "gw:" + hex.EncodeToString(sum[:20])
	if got, err := s.ResolveExecutionSession(ctx, p.User.ID, orgID, id); err == nil {
		return got, nil
	}
	return s.RegisterExecutionSession(ctx, p.User.ID, orgID, id)
}
