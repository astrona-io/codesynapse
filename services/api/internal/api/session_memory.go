package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/sessionmemory"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
	"github.com/astrona-io/codesynapse/services/api/internal/offload"
	"github.com/astrona-io/codesynapse/services/api/internal/recall"
)

func (s *Server) sessionMemoryRoutes() {
	s.route("POST /api/v1/repos/{repo}/memory/index", repoRole(accounts.Member), s.withExecution(s.handleIndexRepository))
	s.route("GET /api/v1/repos/{repo}/session-memory", repoRole(accounts.Viewer), s.withExecution(s.handleSessionMemory))
	s.route("POST /api/v1/repos/{repo}/session-memory", repoRole(accounts.Member), s.withExecution(s.handleWriteSessionMemory))
	s.route("DELETE /api/v1/repos/{repo}/session-memory", repoRole(accounts.Member), s.withExecution(s.handleDeleteSessionMemory))
	s.route("POST /api/v1/repos/{repo}/session-memory/recall", repoRole(accounts.Viewer), s.withExecution(s.handleSessionRecall))
	s.route("POST /api/v1/repos/{repo}/session-memory/offload", repoRole(accounts.Member), s.withExecution(s.handleSessionOffload))
	s.route("POST /api/v1/repos/{repo}/session-memory/promote", repoRole(accounts.Member), s.withExecution(s.handlePromoteSessionMemory))
}

func (s *Server) sessionScope(r *http.Request) (sessionmemory.Scope, store.SessionMemoryStore, error) {
	id, err := execution.Require(r.Context())
	if err != nil {
		return sessionmemory.Scope{}, nil, err
	}
	st, ok := s.st.(store.SessionMemoryStore)
	if !ok {
		return sessionmemory.Scope{}, nil, errors.New("session memory is unavailable")
	}
	scope := sessionmemory.Scope{TenantID: id.TenantID, SessionID: id.SessionID, Namespace: memory.Namespace(id.TenantID, r.PathValue("repo"))}
	return scope, st, scope.Check(r.Context())
}

func (s *Server) sessionMemoryError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sessionmemory.ErrClosed):
		s.problem(w, http.StatusGone, err)
	case errors.Is(err, sessionmemory.ErrLimit):
		s.problem(w, http.StatusBadRequest, err)
	case errors.Is(err, execution.ErrMissing), errors.Is(err, execution.ErrInvalid), errors.Is(err, sessionmemory.ErrScope):
		s.problem(w, http.StatusForbidden, err)
	default:
		s.problem(w, http.StatusInternalServerError, err)
	}
}

func (s *Server) handleSessionMemory(w http.ResponseWriter, r *http.Request) {
	scope, st, err := s.sessionScope(r)
	if err != nil {
		s.sessionMemoryError(w, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit")) //nolint:errcheck // invalid values use the default
	g, err := st.ReadSessionMemory(r.Context(), scope, r.URL.Query().Get("q"), store.ClampLimit(limit, 100))
	if err != nil {
		s.sessionMemoryError(w, err)
		return
	}
	telemetry.RecordStep(r.Context(), telemetry.StepRecallFacts, true)
	telemetry.RecordStepTokens(r.Context(), telemetry.StepRecallFacts, telemetry.KindContextIn, telemetry.EstimateTokens(factBytes(g)))
	writeJSON(w, http.StatusOK, g)
}

// factBytes is the text size of a recalled graph as the hook renders it (one
// line a fact), for the flow's token estimate.
func factBytes(g memory.Graph) int {
	n := 0
	for _, f := range g.Facts {
		n += len(f.Subject) + len(f.Predicate) + len(f.Object) + 16
	}
	return n
}

func (s *Server) handleWriteSessionMemory(w http.ResponseWriter, r *http.Request) {
	scope, st, err := s.sessionScope(r)
	if err != nil {
		s.sessionMemoryError(w, err)
		return
	}
	var in struct {
		Triples []memory.Triple `json:"triples"`
		Source  string          `json:"source"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	record, err := s.cfg.Accounts.St.ExecutionSessionByID(r.Context(), scope.SessionID)
	if err != nil {
		s.accountError(w, err)
		return
	}
	if _, err := memory.Prepare(scope.Key(), in.Triples, in.Source, time.Now()); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	_, authorize := s.jobAuthorization(r.Context(), r.PathValue("repo"), accounts.Member)
	if err := checkJob(r.Context(), authorize); err != nil {
		s.accountError(w, err)
		return
	}
	if err := st.UpsertSessionMemory(r.Context(), scope, in.Triples, in.Source, record.ExpiresAt); err != nil {
		s.sessionMemoryError(w, err)
		return
	}
	s.indexSession(r.Context(), scope, authorize)
	writeJSON(w, http.StatusOK, map[string]int{"accepted": len(in.Triples)})
}

func (s *Server) indexSession(ctx context.Context, scope sessionmemory.Scope, authorize func(context.Context) error) {
	if err := s.recall.IndexSession(ctx, scope, authorize); err != nil && !errors.Is(err, llm.ErrNoEmbeddings) {
		s.logger.Warn("session embedding deferred", "error", err)
	}
}

func (s *Server) handleDeleteSessionMemory(w http.ResponseWriter, r *http.Request) {
	scope, st, err := s.sessionScope(r)
	if err != nil {
		s.sessionMemoryError(w, err)
		return
	}
	if err := st.DeleteSessionMemory(r.Context(), scope); err != nil {
		s.sessionMemoryError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSessionRecall(w http.ResponseWriter, r *http.Request) {
	scope, _, err := s.sessionScope(r)
	if err != nil {
		s.sessionMemoryError(w, err)
		return
	}
	var q recall.Query
	if err := decodeJSON(w, r, &q); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	if len(q.Text) > 2000 || q.History {
		s.problem(w, http.StatusBadRequest, errors.New("session recall accepts at most 2000 query bytes and current facts only"))
		return
	}
	result, err := s.recall.RecallSession(r.Context(), scope, q)
	if err != nil {
		s.sessionMemoryError(w, err)
		return
	}
	// Tokens only for what the hook can add to the chat; a skipped or empty
	// lookup adds nothing.
	telemetry.RecordStep(r.Context(), telemetry.StepRecallHook, true)
	telemetry.RecordStepTokens(r.Context(), telemetry.StepRecallHook, telemetry.KindContextIn, telemetry.EstimateTokens(factBytes(result.Graph)))
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleSessionOffload(w http.ResponseWriter, r *http.Request) {
	scope, _, err := s.sessionScope(r)
	if err != nil {
		s.sessionMemoryError(w, err)
		return
	}
	var in offloadRequest
	if err := decodeJSON(w, r, &in); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	record, err := s.cfg.Accounts.St.ExecutionSessionByID(r.Context(), scope.SessionID)
	if err != nil {
		s.accountError(w, err)
		return
	}
	_, authorize := s.jobAuthorization(r.Context(), r.PathValue("repo"), accounts.Member)
	job, err := s.offload.SubmitSession(r.Context(), r.PathValue("repo"), scope.Namespace, in.Source, in.Turns, record.ExpiresAt, authorize)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, offload.ErrNoLocalModel) {
			status = http.StatusConflict
		}
		if errors.Is(err, offload.ErrBusy) {
			status = http.StatusServiceUnavailable
		}
		s.problem(w, status, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) handlePromoteSessionMemory(w http.ResponseWriter, r *http.Request) {
	scope, st, err := s.sessionScope(r)
	if err != nil {
		s.sessionMemoryError(w, err)
		return
	}
	var in struct {
		Facts []string `json:"fact_ids"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	if len(in.Facts) == 0 || len(in.Facts) > memory.MaxTriplesPerCall {
		s.problem(w, http.StatusBadRequest, errors.New("select 1 to 50 session facts to promote"))
		return
	}
	g, err := st.ReadSessionMemory(r.Context(), scope, "", sessionmemory.MaxFacts)
	if err != nil {
		s.sessionMemoryError(w, err)
		return
	}
	names := map[string]memory.Entity{}
	for _, e := range g.Entities {
		names[e.ID] = e
	}
	facts := map[string]memory.Fact{}
	for _, f := range g.Facts {
		facts[f.ID] = f
	}
	triples := []memory.Triple{}
	seen := map[string]bool{}
	for _, id := range in.Facts {
		f, ok := facts[id]
		if !ok {
			s.problem(w, http.StatusNotFound, errors.New("no such session fact"))
			return
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		a, b := names[f.Subject], names[f.Object]
		triples = append(triples, memory.Triple{Subject: a.Name, SubjectType: a.Type, Predicate: f.Predicate, Object: b.Name, ObjectType: b.Type, Confidence: f.Confidence})
	}
	ns, err := s.memNamespace(r, r.PathValue("repo"))
	if err != nil {
		s.accountError(w, err)
		return
	}
	batch, err := memory.Prepare(ns, triples, "session promotion", time.Now())
	if err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	_, authorize := s.jobAuthorization(r.Context(), r.PathValue("repo"), accounts.Member)
	if err := checkJob(r.Context(), authorize); err != nil {
		s.accountError(w, err)
		return
	}
	if err := s.st.UpsertMemory(r.Context(), batch); err != nil {
		s.sessionMemoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"promoted": len(triples)})
}

func (s *Server) handleIndexRepository(w http.ResponseWriter, r *http.Request) {
	ns, err := s.memNamespace(r, r.PathValue("repo"))
	if err != nil {
		s.accountError(w, err)
		return
	}
	_, authorize := s.jobAuthorization(r.Context(), r.PathValue("repo"), accounts.Member)
	n, err := s.recall.IndexRepository(r.Context(), ns, authorize)
	if errors.Is(err, llm.ErrNoEmbeddings) {
		writeJSON(w, http.StatusOK, map[string]int{"indexed": 0})
		return
	}
	if err != nil {
		s.sessionMemoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"indexed": n})
}
