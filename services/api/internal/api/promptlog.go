package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/auditarchive"
)

// maxPromptLogExport bounds the rows one export streams; a longer range is
// exported in parts (the answer says where it stopped).
const maxPromptLogExport = 20000

// handlePromptLogSettings reads (any member: they may see whether their
// prompts are kept) or sets (owner or admin, signed-in session only, audited
// before the write) the organisation's gateway prompt log.
func (s *Server) handlePromptLogSettings(w http.ResponseWriter, r *http.Request) {
	least := accounts.Member
	if r.Method == http.MethodPut {
		least = accounts.Admin
	}
	p, o, ok := s.orgFor(w, r, least)
	if !ok {
		return
	}
	if r.Method == http.MethodPut {
		cur, err := s.cfg.Accounts.PromptLogSettingOf(r.Context(), p, o.ID)
		if err != nil {
			s.accountError(w, err)
			return
		}
		var req struct {
			Enabled       *bool `json:"enabled"`
			RetentionDays *int  `json:"retention_days"`
		}
		if err := decodeJSON(w, r, &req); err != nil {
			s.problem(w, http.StatusBadRequest, err)
			return
		}
		if req.Enabled == nil && req.RetentionDays == nil {
			s.problem(w, http.StatusBadRequest, errors.New("send enabled, retention_days or both"))
			return
		}
		if req.Enabled != nil {
			cur.Enabled = *req.Enabled
		}
		if req.RetentionDays != nil {
			cur.RetentionDays = *req.RetentionDays
		}
		if err := s.cfg.Accounts.SetPromptLog(r.Context(), p, o.ID, cur); err != nil {
			s.accountError(w, err)
			return
		}
	}
	set, err := s.cfg.Accounts.PromptLogSettingOf(r.Context(), p, o.ID)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": set.Enabled, "retention_days": set.RetentionDays,
		"min_days": accounts.MinPromptLogDays, "max_days": accounts.MaxPromptLogDays, "archive": s.cfg.AuditArchive != nil})
}

// promptLogFilter reads the shared filters of the prompt log page and export.
func promptLogFilter(q url.Values) (accounts.PromptLogFilter, error) {
	f := accounts.PromptLogFilter{UserID: q.Get("user"), SessionID: q.Get("session"), Model: q.Get("model"),
		Route: q.Get("route"), Fallthru: q.Get("fallthrough")}
	switch q.Get("shortened") {
	case "":
	case "true", "false":
		v := q.Get("shortened") == "true"
		f.Shortened = &v
	default:
		return f, errors.New("shortened must be true or false")
	}
	for name, dst := range map[string]*time.Time{"since": &f.Since, "until": &f.Until} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return f, errors.New(name + " must be an RFC 3339 time, e.g. 2026-10-01T00:00:00Z")
			}
			*dst = t
		}
	}
	for name, dst := range map[string]*int64{"before_id": &f.BeforeID, "after_id": &f.AfterID} {
		if v := q.Get(name); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				return f, errors.New(name + " must be a positive number")
			}
			*dst = n
		}
	}
	return f, nil
}

// handlePromptLog lists the organisation's prompt log, newest first, for an
// owner or admin in a signed-in session; every view is audited
// (gateway.prompt_log.read) before a row is read. Other organisations answer 404.
func (s *Server) handlePromptLog(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.Admin)
	if !ok {
		return
	}
	f, err := promptLogFilter(r.URL.Query())
	if err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	f.Limit = 50
	if n := atoi(r.URL.Query().Get("limit")); n > 0 {
		f.Limit = min(n, accounts.MaxPromptLogPage)
	}
	rows, err := s.cfg.Accounts.PromptLog(r.Context(), p, o.ID, accounts.ActionPromptLogRead, f)
	if err != nil {
		s.accountError(w, err)
		return
	}
	var next int64
	if len(rows) == f.Limit {
		next = rows[len(rows)-1].ID
	}
	if rows == nil {
		rows = []accounts.PromptLogEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": rows, "next_before_id": next})
}

// handlePromptLogExport streams the organisation's prompt log for a time
// range as JSON Lines or CSV (formulas neutralised), oldest first, for an
// owner or admin in a signed-in session; audited (gateway.prompt_log.export)
// before a row is read. Each row carries the ID and digests of its audit
// entry, so it can be checked against the chain.
func (s *Server) handlePromptLogExport(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.Admin)
	if !ok {
		return
	}
	q := r.URL.Query()
	format := q.Get("format")
	if format != "jsonl" && format != "csv" {
		s.problem(w, http.StatusBadRequest, errors.New("format must be jsonl or csv"))
		return
	}
	f, err := promptLogFilter(q)
	if err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	if f.Since.IsZero() {
		s.problem(w, http.StatusBadRequest, errors.New("since is required: export a time range"))
		return
	}
	f.BeforeID, f.Ascending, f.Limit = 0, true, accounts.MaxPromptLogPage
	if err := s.cfg.Accounts.AllowPromptLog(r.Context(), p, o.ID, accounts.ActionPromptLogExport, f); err != nil {
		s.accountError(w, err)
		return
	}
	f.OrgID = o.ID
	var rows []accounts.PromptLogEntry
	for len(rows) < maxPromptLogExport {
		page, err := s.cfg.Accounts.St.ListPromptLog(r.Context(), f)
		if err != nil {
			s.problem(w, http.StatusInternalServerError, errors.New("the prompt log could not be read"))
			return
		}
		rows = append(rows, page...)
		if len(page) < f.Limit {
			break
		}
		f.AfterID = page[len(page)-1].ID
	}
	if len(rows) > maxPromptLogExport {
		rows = rows[:maxPromptLogExport]
	}
	if len(rows) == maxPromptLogExport {
		// The client continues with after_id from here.
		w.Header().Set("X-CodeSynapse-Next-After-Id", strconv.FormatInt(rows[len(rows)-1].ID, 10))
	}
	w.Header().Set("Cache-Control", "no-store")
	if format == "jsonl" {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", `attachment; filename="prompt-log.jsonl"`)
		enc := json.NewEncoder(w)
		for _, e := range rows {
			if err := enc.Encode(e); err != nil {
				return
			}
		}
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="prompt-log.csv"`)
	writePromptLogCSV(w, rows)
}

// prunePromptLog applies each organisation's prompt log retention every
// hour (retention can be a single day), under the api-loops lease like the
// audit log's: rows past retention go, and with an audit archive rows past
// the hot window are sealed into files first (auditarchive.PromptLogRetention).
func (s *Server) prunePromptLog(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		s.applyPromptLogRetention(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) applyPromptLogRetention(ctx context.Context, now time.Time) {
	res, err := auditarchive.PromptLogRetention(ctx, s.cfg.Accounts.St, s.cfg.AuditArchive, s.cfg.AuditHot, now)
	if err != nil {
		s.logger.Error("prompt log retention failed", "error", err)
	}
	if res != (auditarchive.PromptLogResult{}) {
		s.logger.Info("prompt log retention", "pruned", res.Pruned, "sealed", res.Sealed, "expired", res.Expired)
	}
}

var promptLogCSVHeader = []string{"id", "at", "audit_id", "user_id", "key_id", "session_id", "path", "model", "route", "status", "duration_ms",
	"shortened", "fallthrough", "prompt_sha256", "prompt_bytes", "forwarded_sha256", "forwarded_bytes", "prompt", "forwarded", "answer"}

func writePromptLogCSV(w http.ResponseWriter, rows []accounts.PromptLogEntry) {
	cw := csv.NewWriter(w)
	_ = cw.Write(promptLogCSVHeader) //nolint:errcheck // client went away
	for _, e := range rows {
		_ = cw.Write([]string{strconv.FormatInt(e.ID, 10), e.At.Format(time.RFC3339Nano), strconv.FormatInt(e.AuditID, 10), //nolint:errcheck // as above
			csvSafe(e.UserID), csvSafe(e.KeyID), csvSafe(e.SessionID), csvSafe(e.Path), csvSafe(e.Model), e.Route, strconv.Itoa(e.Status),
			strconv.FormatInt(e.DurationMS, 10), strconv.FormatBool(e.Shortened), csvSafe(e.Fallthrough), e.PromptSHA256, strconv.Itoa(e.PromptBytes),
			e.ForwardedSHA256, strconv.Itoa(e.ForwardedBytes), csvSafe(e.Prompt), csvSafe(e.Forwarded), csvSafe(e.Answer)})
	}
	cw.Flush()
}
