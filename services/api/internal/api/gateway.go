package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/execution"
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

// gatewayRoutes mounts the agent gateway when the operator turned it on.
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
	o := &gateway.OpenAI{Local: g.Local}
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
