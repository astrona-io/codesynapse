package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/services/api/internal/gateway"
)

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
	g.Done = recordGateway
	g.Upstream = s.cfg.GatewayUpstream
	g.Local = func() *gateway.Local {
		if s.cfg.LLM == nil {
			return nil
		}
		t, ok := s.cfg.LLM.GatewayTarget()
		if !ok {
			return nil
		}
		return &gateway.Local{BaseURL: t.BaseURL, Model: t.Model, APIKey: t.APIKey, Acquire: t.Acquire}
	}
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		h := g.Handler(path)
		s.route("POST "+path, orgRole(accounts.Member), func(w http.ResponseWriter, r *http.Request) {
			r, ok := s.gatewaySession(w, r)
			if !ok {
				return
			}
			h(w, r)
		})
	}
	o := &gateway.OpenAI{Local: g.Local, Done: recordGateway}
	bound := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r, ok := s.gatewaySession(w, r)
			if !ok {
				return
			}
			h(w, r)
		}
	}
	s.route("POST /v1/chat/completions", orgRole(accounts.Member), bound(o.ChatHandler()))
	s.route("GET /v1/models", orgRole(accounts.Member), bound(o.ModelsHandler()))
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
	return r.WithContext(ctx), true
}

// handleOrgGateway reads (any member) or sets (owner or admin, signed-in
// session only) whether the organisation uses the agent gateway (on unless
// switched off). "available"
// says whether the operator turned the gateway on for the installation.
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
			Enabled bool `json:"enabled"`
		}
		if err := decodeJSON(w, r, &req); err != nil {
			s.problem(w, http.StatusBadRequest, err)
			return
		}
		if err := s.cfg.Accounts.SetGateway(r.Context(), p, o.ID, req.Enabled); err != nil {
			s.accountError(w, err)
			return
		}
	}
	on, err := s.cfg.Accounts.GatewaySetting(r.Context(), p, o.ID)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": on, "available": s.cfg.Gateway})
}

// recordGateway counts one gateway request. Only the route, the status class
// and sizes become metrics: never the model name, path or text.
func recordGateway(ctx context.Context, m gateway.Meta, status int, _ time.Duration) {
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
	if route != "local" {
		return
	}
	if !m.Metered {
		telemetry.GatewayLocalUnmetered.IncContext(ctx, tenant)
		return
	}
	telemetry.GatewayLocalTokens.AddContext(ctx, float64(m.InTokens), tenant, "input")
	telemetry.GatewayLocalTokens.AddContext(ctx, float64(m.OutTokens), tenant, "output")
}
