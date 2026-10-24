package api

import (
	"net/http"

	"github.com/astrona-io/codesynapse/internal/agentrules"
)

// The ruleset agent sessions start with (internal/agentrules). The plugin's
// SessionStart hook fetches it so the wording can be improved on the server
// for every machine at once; the built-in text is the fallback there.

func (s *Server) agentRulesRoutes() {
	s.route("GET /api/v1/agent-rules", anyone, s.handleAgentRules)
}

func (s *Server) handleAgentRules(w http.ResponseWriter, _ *http.Request) {
	text, custom := agentrules.Load(s.cfg.AgentRulesFile)
	version := agentrules.Version
	if custom {
		version = 0 // the operator's own wording has no built-in version
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": version, "custom": custom, "text": text})
}
