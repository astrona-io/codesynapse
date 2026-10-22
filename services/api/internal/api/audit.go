package api

import (
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/accounts"
)

// requestIDRe accepts client-supplied request IDs that are safe to log.
var requestIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{8,64}$`)

func requestID(r *http.Request) string {
	if id := r.Header.Get("X-Request-ID"); requestIDRe.MatchString(id) {
		return id
	}
	b := make([]byte, 12)
	_, _ = rand.Read(b) //nolint:errcheck // crypto/rand does not fail on supported platforms
	return hex.EncodeToString(b)
}

func mutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// statusRecorder remembers the status a handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Flush keeps streaming responses (audit export) working.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// auditRequest records a changing request after it ran. Account actions also
// record a semantic entry from the service; this one is the request-level
// trail that covers every endpoint, including failures.
func (s *Server) auditRequest(r *http.Request, pattern string, status int) {
	if pattern == "" {
		pattern = r.Method + " (no route)"
	}
	e := accounts.Event{Action: "http " + pattern, Target: r.URL.Path, Status: status, Outcome: accounts.Success, Via: "anonymous"}
	if status >= 400 {
		e.Outcome = accounts.Failure
	}
	if p, ok := principal(r.Context()); ok {
		e.UserID, e.ActorEmail, e.Via, e.KeyID, e.OrgID = p.User.ID, p.User.Email, p.Via, p.KeyID, p.OrgID
	}
	s.record(r.Context(), e)
}

// deniedLimit bounds how many refused requests per client address reach the
// database each minute; the log sink sees all of them.
var deniedLimit = struct {
	sync.Mutex
	seen map[string][]time.Time
}{seen: map[string][]time.Time{}}

func (s *Server) auditDenied(r *http.Request, status int) {
	e := accounts.Event{Via: "anonymous", Action: "auth.denied", Target: r.Method + " " + r.URL.Path, Outcome: accounts.Failure, Status: status, At: time.Now()}
	ri := accounts.RequestInfoFrom(r.Context())
	e.RequestID, e.IP, e.UserAgent = ri.RequestID, ri.IP, ri.UserAgent
	now := time.Now()
	deniedLimit.Lock()
	recent := deniedLimit.seen[ri.IP][:0]
	for _, t := range deniedLimit.seen[ri.IP] {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	store := len(recent) < 10
	if store {
		recent = append(recent, now)
	}
	deniedLimit.seen[ri.IP] = recent
	if len(deniedLimit.seen) > 100_000 {
		deniedLimit.seen = map[string][]time.Time{}
	}
	deniedLimit.Unlock()
	if !store {
		s.logAudit(e)
		return
	}
	s.record(r.Context(), e)
}

// record stores an entry (accounts mode) or only logs it (single-tenant).
func (s *Server) record(ctx context.Context, e accounts.Event) {
	if s.cfg.Accounts == nil {
		ri := accounts.RequestInfoFrom(ctx)
		e.RequestID, e.IP, e.UserAgent, e.At = ri.RequestID, ri.IP, ri.UserAgent, time.Now().UTC()
		if e.Via == "anonymous" {
			e.Via = "service" // single-tenant mode: everything behind the one token
		}
		s.logAudit(e)
		return
	}
	if _, err := s.cfg.Accounts.Record(ctx, e); err != nil {
		s.logger.Error("audit log write failed", "action", e.Action, "error", err)
	}
}

// logAudit writes an entry as a structured log line for log collectors.
func (s *Server) logAudit(e accounts.Event) {
	s.logger.Info("audit", "audit_id", e.ID, "at", e.At.Format(time.RFC3339Nano), "action", e.Action, "outcome", e.Outcome,
		"status", e.Status, "actor", e.ActorEmail, "user_id", e.UserID, "via", e.Via, "key_id", e.KeyID, "org_id", e.OrgID,
		"target", e.Target, "ip", e.IP, "user_agent", e.UserAgent, "request_id", e.RequestID, "detail", e.Detail, "hash", e.Hash)
}

// pruneAudit applies the retention period once a day.
func (s *Server) pruneAudit(ctx context.Context) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		before := time.Now().Add(-s.cfg.AuditRetention)
		if n, err := s.cfg.Accounts.St.PruneEvents(ctx, before); err != nil {
			s.logger.Warn("pruning the audit log failed", "error", err)
		} else if n > 0 {
			s.record(ctx, accounts.Event{Via: "service", Action: "audit.prune", Detail: strconv.Itoa(n) + " entries older than " + before.Format(time.DateOnly)})
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// handleOrgAudit exports an organisation's audit log (org admins).
func (s *Server) handleOrgAudit(w http.ResponseWriter, r *http.Request) {
	_, o, ok := s.orgFor(w, r, accounts.Admin)
	if !ok {
		return
	}
	s.exportAudit(w, r, o.ID)
}

// handleAdminAudit exports the whole audit log (platform admins).
func (s *Server) handleAdminAudit(w http.ResponseWriter, r *http.Request) {
	if !s.platformAdmin(w, r) {
		return
	}
	s.exportAudit(w, r, "")
}

func (s *Server) handleVerifyAudit(w http.ResponseWriter, r *http.Request) {
	if !s.platformAdmin(w, r) {
		return
	}
	v, err := accounts.VerifyAudit(r.Context(), s.cfg.Accounts.St)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	writeJSON(w, 200, v)
}

// exportAudit writes entries as JSON (default), JSON Lines or CSV. Query:
// since, until (RFC 3339), after_id (cursor), limit (max 10000), action
// (prefix), actor (email or user ID), outcome (success, failure), format.
func (s *Server) exportAudit(w http.ResponseWriter, r *http.Request, orgID string) {
	q := r.URL.Query()
	f := accounts.EventFilter{OrgID: orgID, Limit: clamp(atoi(q.Get("limit")), 1, 10000, 500), Ascending: q.Get("order") == "asc",
		Action: q.Get("action"), Actor: accounts.NormalizeEmail(q.Get("actor")), Outcome: q.Get("outcome")}
	if f.Outcome != "" && f.Outcome != accounts.Success && f.Outcome != accounts.Failure {
		s.problem(w, 400, errors.New("outcome must be success or failure"))
		return
	}
	if len(f.Action) > 100 || len(f.Actor) > 320 {
		s.problem(w, 400, errors.New("filter too long"))
		return
	}
	var err error
	if f.AfterID, err = strconv.ParseInt(cmpOr(q.Get("after_id"), "0"), 10, 64); err != nil {
		s.problem(w, 400, errors.New("after_id must be a number"))
		return
	}
	for name, dst := range map[string]*time.Time{"since": &f.Since, "until": &f.Until} {
		if v := q.Get(name); v != "" {
			if *dst, err = time.Parse(time.RFC3339, v); err != nil {
				s.problem(w, 400, errors.New(name+" must be an RFC 3339 time, e.g. 2026-10-01T00:00:00Z"))
				return
			}
		}
	}
	events, err := s.cfg.Accounts.St.ListEvents(r.Context(), f)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	switch q.Get("format") {
	case "jsonl":
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", `attachment; filename="audit.jsonl"`)
		enc := json.NewEncoder(w)
		for _, e := range events {
			if err := enc.Encode(e); err != nil {
				return
			}
		}
	case "csv":
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="audit.csv"`)
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"id", "at", "action", "outcome", "status", "actor_email", "user_id", "via", "key_id", "org_id", "target", "ip", "user_agent", "request_id", "detail", "prev_hash", "hash"}) //nolint:errcheck // client went away
		for _, e := range events {
			_ = cw.Write([]string{strconv.FormatInt(e.ID, 10), e.At.Format(time.RFC3339Nano), e.Action, e.Outcome, strconv.Itoa(e.Status), //nolint:errcheck // as above
				csvSafe(e.ActorEmail), e.UserID, e.Via, e.KeyID, e.OrgID, csvSafe(e.Target), e.IP, csvSafe(e.UserAgent), e.RequestID, csvSafe(e.Detail), e.PrevHash, e.Hash})
		}
		cw.Flush()
	default:
		writeJSON(w, 200, map[string]any{"events": events})
	}
}

// csvSafe defuses spreadsheet formulas in free text (CSV injection).
func csvSafe(s string) string {
	if s != "" && (s[0] == '=' || s[0] == '+' || s[0] == '-' || s[0] == '@') {
		return "'" + s
	}
	return s
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n } //nolint:errcheck // bad values fall back to defaults

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
