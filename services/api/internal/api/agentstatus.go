package api

import (
	"net/http"
	"time"

	"github.com/astrona-io/codesynapse/internal/usage"
)

// AgentStatus is GET /api/v1/agents/status: what the caller's organisations'
// agents did today, cheap enough for the Agents page to poll while someone
// connects an agent. It reads the same counters as Statistics, so a call
// shows up within a minute (the MCP server flushes every 30 seconds).
type AgentStatus struct {
	Sessions int `json:"sessions"` // MCP sessions started today
	Calls    int `json:"calls"`    // tool and prompt calls today
	// Workflow is how many sessions reported how they began, which only the
	// plugin's hooks do: it says the plugin is installed and running.
	Workflow int `json:"workflow"`
}

func (s *Server) handleAgentStatus(w http.ResponseWriter, r *http.Request) {
	buckets, err := s.usageBuckets(r.Context(), 1, time.Now())
	if err != nil {
		s.problem(w, http.StatusInternalServerError, err)
		return
	}
	var st AgentStatus
	var tokens usage.TokenCounts
	var edits usage.EditCounts
	for _, b := range buckets {
		switch {
		// Token and edit counters share the buckets; their Calls is a size.
		case tokens.AddCounter(b.Tool, b.Calls), edits.AddCounter(b.Tool, b.Calls):
		case b.Tool == usage.Session:
			st.Sessions += b.Calls
		case b.Tool == usage.FirstCodeSynapse || b.Tool == usage.FirstNative:
			st.Workflow += b.Calls
		case usage.IsWorkflow(b.Tool):
		default:
			st.Calls += b.Calls
		}
	}
	writeJSON(w, http.StatusOK, st)
}
