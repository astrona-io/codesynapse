package api

import (
	"cmp"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// Agent sessions (docs/PLAN.md, "Agent session timeline"): what an agent's
// chat asked CodeSynapse, in order, read from the MCP audit log. Someone
// reviewing an agent's pull request sees what it looked at and how much it
// got back. Members see their own sessions; owners and admins, who can
// already read the organisation's audit log, see everyone's.

const (
	maxSessionEvents = 10000 // scanned for the list
	maxSessionList   = 200
	maxSessionCalls  = 2000
)

func (s *Server) agentSessionRoutes() {
	s.account("GET /api/v1/accounts/orgs/{org}/agent-sessions", businessRoute, s.handleAgentSessions)
	s.account("GET /api/v1/accounts/orgs/{org}/agent-sessions/{session}", businessRoute, s.handleAgentSession)
}

// AgentSession summarises one runtime (chat) session.
type AgentSession struct {
	ID     string `json:"id"`
	Actor  string `json:"actor"`
	Client string `json:"client,omitempty"`
	// Key is the name of the API key the chat used; empty when it was
	// deleted since, or the entry carries none.
	Key        string         `json:"key,omitempty"`
	First      time.Time      `json:"first"`
	Last       time.Time      `json:"last"`
	Calls      int            `json:"calls"`
	Errors     int            `json:"errors"`
	Bytes      int64          `json:"bytes"`
	DurationMS int64          `json:"duration_ms"`
	Tools      map[string]int `json:"tools"`
	Repos      []string       `json:"repos"`
}

// SessionCall is one request of a session.
type SessionCall struct {
	At         time.Time         `json:"at"`
	Method     string            `json:"method"`
	Name       string            `json:"name,omitempty"`
	Repo       string            `json:"repo,omitempty"`
	Outcome    string            `json:"outcome"`
	Error      string            `json:"error,omitempty"`
	DurationMS int64             `json:"duration_ms"`
	Bytes      int64             `json:"bytes"`
	Args       map[string]string `json:"args,omitempty"`
}

// agentSessionScope is the organisation and the actor filter of the caller: ""
// (everyone) for owners and admins, the caller's own user otherwise.
func (s *Server) agentSessionScope(w http.ResponseWriter, r *http.Request) (accounts.Org, string, bool) {
	p, o, ok := s.orgFor(w, r, accounts.Member)
	if !ok {
		return o, "", false
	}
	if s.cfg.Accounts.Require(r.Context(), p, o.ID, accounts.Admin) == nil {
		return o, "", true
	}
	return o, p.User.ID, true
}

func (s *Server) handleAgentSessions(w http.ResponseWriter, r *http.Request) {
	o, actor, ok := s.agentSessionScope(w, r)
	if !ok {
		return
	}
	days := clamp(atoi(r.URL.Query().Get("days")), 1, 30, 7)
	events, err := s.cfg.Accounts.St.ListEvents(r.Context(), accounts.EventFilter{
		OrgID: o.ID, Action: "mcp.", Actor: actor, Since: time.Now().Add(-time.Duration(days) * 24 * time.Hour), Limit: maxSessionEvents,
	})
	if err != nil {
		s.problem(w, http.StatusInternalServerError, err)
		return
	}
	keys := s.keyNames(r, events)
	byID := map[string]*AgentSession{}
	for _, e := range events {
		if e.MCP == nil || e.MCP.SessionID == "" {
			continue
		}
		a := byID[e.MCP.SessionID]
		if a == nil {
			a = &AgentSession{ID: e.MCP.SessionID, Actor: e.ActorEmail, First: e.At, Last: e.At, Tools: map[string]int{}, Repos: []string{}}
			byID[e.MCP.SessionID] = a
		}
		a.First, a.Last = minTime(a.First, e.At), maxTime(a.Last, e.At)
		a.Client = cmp.Or(a.Client, e.MCP.Client)
		a.Key = cmp.Or(a.Key, keys[e.KeyID])
		a.Calls++
		a.Bytes += e.MCP.Bytes
		a.DurationMS += e.MCP.DurationMS
		if e.Outcome == accounts.Failure {
			a.Errors++
		}
		if e.MCP.Name != "" {
			a.Tools[e.MCP.Name]++
		}
		if e.MCP.Repo != "" && !slices.Contains(a.Repos, e.MCP.Repo) {
			a.Repos = append(a.Repos, e.MCP.Repo)
		}
	}
	out := make([]AgentSession, 0, len(byID))
	for _, a := range byID {
		slices.Sort(a.Repos)
		out = append(out, *a)
	}
	slices.SortFunc(out, func(x, y AgentSession) int { return cmp.Or(y.Last.Compare(x.Last), cmp.Compare(x.ID, y.ID)) })
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions": out[:min(len(out), maxSessionList)],
		"days":     days,
		// The scan stopped at its bound: older sessions may be cut short.
		"truncated": len(events) == maxSessionEvents,
		"scope":     map[bool]string{true: "organisation", false: "own"}[actor == ""],
	})
}

func (s *Server) handleAgentSession(w http.ResponseWriter, r *http.Request) {
	o, actor, ok := s.agentSessionScope(w, r)
	if !ok {
		return
	}
	id := r.PathValue("session")
	if id == "" || len(id) > 128 {
		s.problem(w, http.StatusBadRequest, errors.New("session must be 1 to 128 characters"))
		return
	}
	events, err := s.cfg.Accounts.St.ListEvents(r.Context(), accounts.EventFilter{
		OrgID: o.ID, Action: "mcp.", Actor: actor, Session: id, Ascending: true, Limit: maxSessionCalls + 1,
	})
	if err != nil {
		s.problem(w, http.StatusInternalServerError, err)
		return
	}
	if len(events) == 0 {
		s.problem(w, http.StatusNotFound, errors.New("no such session in this organisation"))
		return
	}
	calls := make([]SessionCall, 0, min(len(events), maxSessionCalls))
	for _, e := range events[:min(len(events), maxSessionCalls)] {
		c := SessionCall{At: e.At, Outcome: e.Outcome}
		if m := e.MCP; m != nil {
			c.Method, c.Name, c.Repo, c.Error, c.DurationMS, c.Bytes, c.Args = m.Method, m.Name, m.Repo, m.Error, m.DurationMS, m.Bytes, m.Args
		}
		calls = append(calls, c)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "actor": events[0].ActorEmail, "key": s.keyNames(r, events[:1])[events[0].KeyID], "calls": calls,
		"truncated": len(events) > maxSessionCalls, "limit": maxSessionCalls,
	})
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// keyNames maps the API key IDs of events to their names. Keys belong to the
// events' users, so it reads those users' keys only; a failed read leaves the
// names out rather than failing the list.
func (s *Server) keyNames(r *http.Request, events []accounts.Event) map[string]string {
	names := map[string]string{}
	seen := map[string]bool{}
	for _, e := range events {
		if e.KeyID == "" || e.UserID == "" || seen[e.UserID] {
			continue
		}
		seen[e.UserID] = true
		keys, err := s.cfg.Accounts.St.ListAPIKeys(r.Context(), e.UserID)
		if err != nil {
			continue
		}
		for _, k := range keys {
			names[k.ID] = k.Name
		}
	}
	return names
}
