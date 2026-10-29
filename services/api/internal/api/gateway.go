package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/services/api/internal/gateway"
)

// maxGatewayInFlight caps the requests one key may have open at once, so a
// single key cannot fill the local model's queue or the server's goroutines.
const maxGatewayInFlight = 8

// keyLimit counts the open gateway requests of each key.
type keyLimit struct {
	mu sync.Mutex
	n  map[string]int
}

// take reserves a slot for the key; ok is false when it already holds its share.
func (l *keyLimit) take(key string) (release func(), ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n == nil {
		l.n = map[string]int{}
	}
	if l.n[key] >= maxGatewayInFlight {
		return nil, false
	}
	l.n[key]++
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.n[key]--; l.n[key] <= 0 {
			delete(l.n, key)
		}
	}, true
}

// modelName keeps the audit trail free of anything but a plain model name.
var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,63}$`)

// fallReason keeps the audit trail to plain fall-through reasons.
var fallReason = regexp.MustCompile(`^[a-z_]{1,32}$`)

// auditGateway records what a gateway call was, never what it said: route,
// model name, sizes, timing and SHA-256 digests of the newest user text and
// of the text forwarded when the gateway shortened it. When the organisation
// keeps a prompt log, the texts go to that separate table, linked to this
// entry by its ID and the same digests.
func (s *Server) auditGateway(ctx context.Context, m gateway.Meta, status int, took time.Duration) {
	recordGateway(ctx, m, status, took)
	model := m.Model
	if !modelName.MatchString(model) {
		model = "other"
	}
	if status == 0 {
		status = http.StatusOK
	}
	e := accounts.Event{
		Action: "gateway.request", Target: m.Path, Status: status, Outcome: accounts.Success, Via: "anonymous",
		Detail: gatewayDetail(m, model, took),
	}
	if status >= 400 {
		e.Outcome = accounts.Failure
	}
	p, ok := principal(ctx)
	if ok {
		e.UserID, e.ActorEmail, e.Via, e.KeyID, e.OrgID = p.User.ID, p.User.Email, p.Via, p.KeyID, p.OrgID
	}
	if s.cfg.Accounts == nil || !gateway.PromptCapture(ctx) || !ok {
		s.record(ctx, e)
		return
	}
	stored, err := s.cfg.Accounts.Record(ctx, e)
	if err != nil {
		// No chain entry, no prompt log row: a row always matches one.
		s.logger.Error("audit log write failed", "action", e.Action, "error", err)
		return
	}
	id, _ := execution.From(ctx)
	row := accounts.PromptLogEntry{
		OrgID: p.OrgID, At: stored.At, AuditID: stored.ID, UserID: p.User.ID, KeyID: p.KeyID,
		SessionID: id.SessionID, Path: m.Path, Model: model, Route: m.Route, Status: status,
		DurationMS: took.Milliseconds(), Shortened: m.TextChanged, Fallthrough: m.Fallthrough,
		PromptSHA256: m.PromptSHA256, PromptBytes: m.PromptBytes, ForwardedSHA256: m.ForwardedSHA256, ForwardedBytes: m.ForwardedBytes,
		Prompt: m.Prompt, Forwarded: m.Forwarded, Answer: m.Answer,
	}
	if _, err := s.cfg.Accounts.RecordPromptLog(ctx, row); err != nil {
		// Only the entry it belongs to is logged; the texts never reach a log.
		s.logger.Error("prompt log write failed", "audit_id", stored.ID)
	}
}

// gatewayDetail is the audit detail of a gateway request: no text, only
// digests and sizes, so a kept prompt can be checked against the chain.
func gatewayDetail(m gateway.Meta, model string, took time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "route=%s model=%s stream=%t tools=%t bytes=%d ms=%d shortened=%t", m.Route, model, m.Stream, m.Tools, m.Bytes, took.Milliseconds(), m.TextChanged)
	if m.PromptSHA256 != "" {
		fmt.Fprintf(&b, " prompt_bytes=%d prompt_sha256=%s", m.PromptBytes, m.PromptSHA256)
	}
	if m.TextChanged && m.ForwardedSHA256 != "" {
		fmt.Fprintf(&b, " fwd_bytes=%d fwd_sha256=%s", m.ForwardedBytes, m.ForwardedSHA256)
	}
	if m.Fallthrough != "" {
		reason := m.Fallthrough
		if !fallReason.MatchString(reason) {
			reason = "other"
		}
		fmt.Fprintf(&b, " fallthrough=%s", reason)
	}
	return b.String()
}

// authenticateGateway reads the CodeSynapse key from its own header and
// leaves Authorization and x-api-key to the caller's model credential, which
// the gateway passes on for that one request. Only API keys sign in here.
func (s *Server) authenticateGateway(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	key := r.Header.Get(gateway.KeyHeader)
	// OpenAI clients only know Authorization: Bearer; those paths answer from
	// the local model, so no model credential travels in that header.
	if key == "" && (r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/models") {
		key, _ = strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	if !strings.HasPrefix(key, accounts.KeyPrefix) {
		s.problem(w, http.StatusUnauthorized, errors.New("send a CodeSynapse gateway key in the "+gateway.KeyHeader+" header"))
		return r, false
	}
	p, err := s.cfg.Accounts.AuthenticateKey(r.Context(), key)
	accounts.RecordAuth(r.Context(), telemetry.AuthAPIKey, p.OrgID, err)
	if err != nil {
		s.problem(w, http.StatusUnauthorized, errors.New("the gateway key is not valid"))
		return r, false
	}
	return r.WithContext(context.WithValue(r.Context(), principalKey{}, p)), true
}

// gatewayRoutes mounts the agent gateway unless the operator switched it off
// (CODESYNAPSE_GATEWAY=false).
// Accounts are required: every request needs a key, an organisation and an
// audited runtime session.
func (s *Server) gatewayRoutes() {
	if !s.cfg.Gateway || s.cfg.Accounts == nil {
		return
	}
	g := gateway.NewAnthropic()
	g.Upstream = s.cfg.GatewayUpstream
	g.Local = func() *gateway.Local {
		if s.cfg.LLM == nil {
			return nil
		}
		t, ok := s.cfg.LLM.GatewayTarget()
		if !ok {
			return nil
		}
		return &gateway.Local{BaseURL: t.BaseURL, Model: t.Model, APIKey: t.APIKey, Acquire: t.Acquire, ContextWindow: t.ContextWindow}
	}
	g.Done = s.auditGateway
	var open keyLimit
	// guard binds the request to the key's session and holds one of the key's
	// slots until the answer is done.
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			p, _ := principal(r.Context())
			r, ok := s.gatewaySession(w, r)
			if !ok {
				return
			}
			release, ok := open.take(p.KeyID)
			if !ok {
				w.Header().Set("Retry-After", "5")
				s.problem(w, http.StatusTooManyRequests, errors.New("this key has too many gateway requests open; wait for one to finish"))
				return
			}
			defer release()
			h(w, r)
		}
	}
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		s.route("POST "+path, orgRole(accounts.Member), guard(g.Handler(path)))
	}
	o := &gateway.OpenAI{Local: g.Local, Done: s.auditGateway}
	s.route("POST /v1/chat/completions", orgRole(accounts.Member), guard(o.ChatHandler()))
	s.route("GET /v1/models", orgRole(accounts.Member), guard(o.ModelsHandler()))
}

// gatewaySession binds the request to the key's own runtime session; headers
// naming another session are ignored, never trusted.
func (s *Server) gatewaySession(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	p, ok := s.caller(w, r)
	if !ok {
		return r, false
	}
	m, err := scopeFrom(r.Context()).selected()
	if err != nil {
		s.problem(w, http.StatusForbidden, errors.New("the gateway needs one organisation"))
		return r, false
	}
	on, err := s.cfg.Accounts.GatewayEnabled(r.Context(), m.Org.ID)
	if err != nil {
		s.accountError(w, err)
		return r, false
	}
	if !on {
		s.problem(w, http.StatusForbidden, errors.New("the gateway is switched off for this organisation; an owner or admin turns it on"))
		return r, false
	}
	first, err := s.cfg.Accounts.GatewayLocalFirst(r.Context(), m.Org.ID)
	if err != nil {
		s.accountError(w, err)
		return r, false
	}
	id, err := s.cfg.Accounts.GatewayExecutionSession(r.Context(), p, m.Org.ID)
	if err != nil {
		s.problem(w, http.StatusForbidden, errors.New("this key cannot use the gateway"))
		return r, false
	}
	ctx, err := execution.With(r.Context(), id)
	if err != nil {
		s.problem(w, http.StatusForbidden, err)
		return r, false
	}
	keep, err := s.cfg.Accounts.PromptLogEnabled(r.Context(), m.Org.ID)
	if err != nil {
		s.accountError(w, err)
		return r, false
	}
	return r.WithContext(gateway.WithPromptCapture(gateway.WithLocalFirst(ctx, first), keep)), true
}

// handleOrgGateway reads (any member) or sets (owner or admin, signed-in
// session only) whether the organisation uses the agent gateway (on unless
// switched off) and whether the local model answers first (on unless switched
// off). A PUT changes only the fields it sends. "available" says whether the
// operator turned the gateway on for the installation.
func (s *Server) handleOrgGateway(w http.ResponseWriter, r *http.Request) {
	least := accounts.Member
	if r.Method == http.MethodPut {
		least = accounts.Admin
	}
	p, o, ok := s.orgFor(w, r, least)
	if !ok {
		return
	}
	if r.Method == http.MethodPut {
		var req struct {
			Enabled    *bool `json:"enabled"`
			LocalFirst *bool `json:"local_first"`
		}
		if err := decodeJSON(w, r, &req); err != nil {
			s.problem(w, http.StatusBadRequest, err)
			return
		}
		if req.Enabled == nil && req.LocalFirst == nil {
			s.problem(w, http.StatusBadRequest, errors.New("send enabled, local_first or both"))
			return
		}
		if req.Enabled != nil {
			if err := s.cfg.Accounts.SetGateway(r.Context(), p, o.ID, *req.Enabled); err != nil {
				s.accountError(w, err)
				return
			}
		}
		if req.LocalFirst != nil {
			if err := s.cfg.Accounts.SetGatewayLocalFirst(r.Context(), p, o.ID, *req.LocalFirst); err != nil {
				s.accountError(w, err)
				return
			}
		}
	}
	set, err := s.cfg.Accounts.GatewaySetting(r.Context(), p, o.ID)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": set.Enabled, "local_first": set.LocalFirst, "available": s.cfg.Gateway})
}

// recordGateway counts one gateway request. Only the route, the status class,
// sizes, time taken and fall-through reason become metrics: never the model
// name, path or text.
func recordGateway(ctx context.Context, m gateway.Meta, status int, took time.Duration) {
	route := m.Route
	if route != "local" && route != "cloud" {
		route = "rejected"
	}
	class := "other"
	switch {
	case status >= 200 && status < 300:
		class = "2xx"
	case status >= 400 && status < 500:
		class = "4xx"
	case status >= 500:
		class = "5xx"
	}
	tenant := telemetry.Tenant(ctx)
	telemetry.GatewayRequests.IncContext(ctx, tenant, route, class)
	telemetry.GatewayBytes.AddContext(ctx, float64(m.Bytes), tenant, route, "in")
	telemetry.GatewayBytes.AddContext(ctx, float64(m.Out), tenant, route, "out")
	telemetry.RecordGatewayDuration(ctx, route, took)
	telemetry.RecordGatewayFallthrough(ctx, m.Fallthrough)
	if route != "local" {
		return
	}
	if !m.Metered {
		telemetry.GatewayLocalUnmetered.IncContext(ctx, tenant)
		return
	}
	telemetry.GatewayLocalTokens.AddContext(ctx, float64(m.InTokens), tenant, "input")
	telemetry.GatewayLocalTokens.AddContext(ctx, float64(m.OutTokens), tenant, "output")
	if m.CachedTokens > 0 {
		telemetry.GatewayLocalTokens.AddContext(ctx, float64(m.CachedTokens), tenant, "cached")
	}
}
