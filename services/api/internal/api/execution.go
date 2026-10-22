package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/store"
)

var errExecutionRegistry = errors.New("execution requires an authenticated runtime session registry; enable accounts before starting jobs")

func (s *Server) executionRoutes() {
	s.route("POST /api/v1/execution/sessions", orgRole(accounts.Guest), s.handleRegisterExecution)
	s.route("DELETE /api/v1/execution/sessions/{session}", orgRole(accounts.Guest), s.handleRevokeExecution)
}

func (s *Server) handleRegisterExecution(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	m, err := scopeFrom(r.Context()).selected()
	if err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	var in struct {
		SessionID string `json:"session_id"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	id, err := s.cfg.Accounts.RegisterExecutionSession(r.Context(), p.User.ID, m.Org.ID, in.SessionID)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, id)
}

func (s *Server) handleRevokeExecution(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	m, err := scopeFrom(r.Context()).selected()
	if err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	if err := s.cfg.Accounts.RevokeExecutionSession(r.Context(), p.User.ID, m.Org.ID, r.PathValue("session")); err != nil {
		s.accountError(w, err)
		return
	}
	record, err := s.cfg.Accounts.St.ExecutionSessionByID(r.Context(), r.PathValue("session"))
	if err != nil {
		s.accountError(w, err)
		return
	}
	if err := s.deleteExecutionMemory(r.Context(), record); err != nil {
		s.problem(w, http.StatusServiceUnavailable, errors.New("session revoked; memory cleanup will retry"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Runtime headers select a registered binding, never an arbitrary memory scope.
func (s *Server) executionRequest(w http.ResponseWriter, r *http.Request, required bool) (*http.Request, bool) {
	primary, parent := r.Header.Values(execution.SessionHeader), r.Header.Values(execution.ParentSessionHeader)
	if len(primary) == 0 && len(parent) == 0 && !required {
		return r, true
	}
	session, err := executionHeader(r)
	if err != nil {
		s.problem(w, http.StatusForbidden, err)
		return r, false
	}
	p, ok := s.caller(w, r)
	if !ok {
		return r, false
	}
	m, err := scopeFrom(r.Context()).selected()
	if err != nil {
		s.problem(w, http.StatusForbidden, errors.New("execution requires one authenticated organisation"))
		return r, false
	}
	return s.bindExecutionRequest(w, r, p.User.ID, m.Org.ID, session)
}

func executionHeader(r *http.Request) (string, error) {
	primary, parent := r.Header.Values(execution.SessionHeader), r.Header.Values(execution.ParentSessionHeader)
	if len(primary) != 1 || len(parent) != 1 || primary[0] == "" || parent[0] != primary[0] {
		return "", errors.New("supply the registered runtime session in X-CodeSynapse-Session and the identical X-CodeSynapse-Parent-Session")
	}
	return primary[0], nil
}

func (s *Server) bindExecutionRequest(w http.ResponseWriter, r *http.Request, userID, orgID, sessionID string) (*http.Request, bool) {
	id, err := s.cfg.Accounts.ResolveExecutionSession(r.Context(), userID, orgID, sessionID)
	if err != nil {
		s.problem(w, http.StatusForbidden, errors.New("execution session is unavailable; register the runtime thread before invoking tools"))
		return r, false
	}
	ctx, err := execution.With(r.Context(), id)
	if err != nil {
		s.problem(w, http.StatusForbidden, err)
		return r, false
	}
	return r.WithContext(ctx), true
}

func (s *Server) withExecution(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r, ok := s.executionRequest(w, r, true)
		if ok {
			next(w, r)
		}
	}
}

// Closed bindings retain tombstones; cleanup can safely retry after a restart.
func (s *Server) deleteExecutionMemory(ctx context.Context, record accounts.ExecutionSession) error {
	st, ok := s.st.(store.SessionMemoryStore)
	if !ok {
		return errors.New("session memory is unavailable")
	}
	id := execution.Identity{TenantID: record.OrgID, SubjectID: record.UserID, SessionID: record.ID, ParentSessionID: record.ID}
	ctx, err := execution.With(ctx, id)
	if err != nil {
		return err
	}
	return st.DeleteExecutionMemory(ctx, record.OrgID, record.ID)
}

func (s *Server) pruneExecutionMemory(ctx context.Context) {
	timer := time.NewTicker(time.Minute)
	defer timer.Stop()
	cursor := ""
	for {
		run, cancel := context.WithTimeout(ctx, 30*time.Second)
		rows, err := s.cfg.Accounts.St.ClosedExecutionSessions(run, time.Now(), cursor, 100)
		if err == nil {
			if len(rows) == 0 {
				cursor = ""
			}
			for _, row := range rows {
				if row.RevokedAt == nil {
					err = s.cfg.Accounts.St.RevokeExecutionSession(run, row.ID, row.OrgID, row.UserID, time.Now())
					if err != nil {
						break
					}
				}
				if err = s.deleteExecutionMemory(run, row); err != nil {
					break
				}
				cursor = row.ID
			}
		}
		cancel()
		if err != nil && ctx.Err() == nil {
			s.logger.Warn("session memory cleanup deferred", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}
