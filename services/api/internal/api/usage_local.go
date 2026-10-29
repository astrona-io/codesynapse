package api

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/asklocal"
	"github.com/astrona-io/codesynapse/internal/coretools"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/fetchsource"
	"github.com/astrona-io/codesynapse/internal/ghtools"
	"github.com/astrona-io/codesynapse/internal/gittools"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/usage"
)

// Calls the Claude Code bridge answers on the developer's machine (the git
// tools read a checkout the server cannot see, and the core tools answer from
// the working tree while it has unsynced changes) are reported here before
// the bridge gives the answer, so they are counted on Statistics and recorded
// in the audit log like the calls the MCP server answers: an mcp.local_answer
// entry with the caller, the chat's runtime session, the client, the tool and
// the repository. The report carries the tool name, the repository the
// checkout is, the time it took and the size of the answer; never arguments,
// paths or contents. The bridge answers locally only when this succeeded, so
// a refused session or an audit log that cannot be written sends the call to
// the MCP server instead.

const (
	maxLocalDurationMs = 10 * 60 * 1000
	maxLocalTokens     = 1_000_000
)

type localUsageReport struct {
	Tool           string `json:"tool"`
	Client         string `json:"client"`
	Repo           string `json:"repo"`
	DurationMs     int    `json:"duration_ms"`
	ResponseTokens int    `json:"response_tokens"`
	Failed         bool   `json:"failed"`
}

func (s *Server) usageLocalRoutes() {
	// mcp_local_answers_total names the tool, so the API accepts the same
	// fixed set as validate; ToolLabel turns anything else into unknown.
	telemetry.AllowTools(localTools()...)
	s.route("POST /api/v1/usage/local", visibleOnly, s.handleLocalUsage)
	s.route("POST /api/v1/usage/unindexed", anyone, s.handleUnindexedUsage)
}

// maxUnindexedPerDay bounds the distinct checkout names one organisation can
// report in a day: names come from the client, so they must not mint rows
// without limit.
const maxUnindexedPerDay = 200

type unindexedReport struct {
	Repo   string `json:"repo"`
	Client string `json:"client"`
}

// handleUnindexedUsage counts a pre-finish check the Stop hook skipped because
// the server has not indexed the checkout (usage.Unindexed). Only the name
// the checkout's origin gives arrives, after validRepoName; nothing about the
// files or the session.
func (s *Server) handleUnindexedUsage(w http.ResponseWriter, r *http.Request) {
	var in unindexedReport
	if err := decodeJSON(w, r, &in); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	if !validRepoName(in.Repo) {
		s.problem(w, http.StatusBadRequest, errors.New("repo must be a repository name such as owner/name"))
		return
	}
	if !clientNameRe.MatchString(in.Client) {
		s.problem(w, http.StatusBadRequest, errors.New("client must be a short lower-case name, e.g. claude-code"))
		return
	}
	org := ""
	if s.cfg.Accounts != nil {
		m, err := scopeFrom(r.Context()).selected()
		if err != nil {
			s.problem(w, http.StatusBadRequest, err)
			return
		}
		org = m.Org.ID
	}
	today := usage.Day(time.Now())
	existing, err := s.st.Usage(r.Context(), today)
	if err != nil {
		s.problem(w, http.StatusInternalServerError, err)
		return
	}
	names := map[string]bool{}
	for _, b := range existing {
		if b.Tool == usage.Unindexed && b.Org == org && b.Day == today {
			names[b.Repo] = true
		}
	}
	if !names[in.Repo] && len(names) >= maxUnindexedPerDay {
		s.problem(w, http.StatusTooManyRequests, errors.New("too many unindexed repositories reported today"))
		return
	}
	b := usage.Bucket{Day: today, Org: org, Repo: in.Repo, Tool: usage.Unindexed, Client: in.Client, Calls: 1}
	if err := s.st.AddUsage(r.Context(), []usage.Bucket{b}); err != nil {
		s.problem(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLocalUsage(w http.ResponseWriter, r *http.Request) {
	var in localUsageReport
	if err := decodeJSON(w, r, &in); err != nil {
		telemetry.RecordLocalAnswer(r.Context(), "", telemetry.LocalRefused)
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	if err := in.validate(); err != nil {
		telemetry.RecordLocalAnswer(r.Context(), in.Tool, telemetry.LocalRefused)
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	// The repository is the bridge's name for the checkout; one the caller
	// cannot read is dropped, not refused, so the git tools of a checkout
	// that is not indexed (or not theirs) keep working.
	repo := in.Repo
	if !scopeFrom(r.Context()).sees(repo) {
		repo = ""
	}
	e := accounts.Event{Action: localAnswerAction, Target: cmp.Or(repo, in.Tool), Outcome: accounts.Success, Status: http.StatusNoContent,
		MCP: &accounts.MCPRequest{Method: "tools/call", Name: in.Tool, Repo: repo, Client: in.Client,
			DurationMS: int64(in.DurationMs), Bytes: int64(in.ResponseTokens) * 4}}
	if in.Failed {
		e.Outcome = accounts.Failure
	}
	if s.cfg.Accounts == nil {
		s.record(r.Context(), e)
		s.countLocal(r, in, "", repo)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// A local answer is audited like a tool call: it needs the chat's
	// registered runtime session, and no entry means no answer.
	er, ok := s.executionRequest(w, r, true)
	if !ok {
		telemetry.RecordLocalAnswer(r.Context(), in.Tool, telemetry.LocalRefused)
		return
	}
	s.auditLocal(w, er, in, e)
}

// auditLocal writes the mcp.local_answer entry of a local call in the
// session's organisation and counts it; w gets 204 only when the entry was
// written.
func (s *Server) auditLocal(w http.ResponseWriter, r *http.Request, in localUsageReport, e accounts.Event) {
	p, _ := principal(r.Context())
	id, err := execution.Require(r.Context())
	if err != nil {
		telemetry.RecordLocalAnswer(r.Context(), in.Tool, telemetry.LocalRefused)
		s.problem(w, http.StatusForbidden, err)
		return
	}
	org := id.TenantID
	repo := e.MCP.Repo
	if reach, ok := scopeFrom(r.Context()).reach[repo]; repo != "" && (!ok || reach.OrgID != org) {
		// Seen through another organisation than the session's.
		repo = ""
		e.Target, e.MCP.Repo = in.Tool, ""
	}
	e.UserID, e.ActorEmail, e.Via, e.KeyID, e.OrgID = p.User.ID, p.User.Email, p.Via, p.KeyID, org
	e.MCP.SessionID = id.SessionID
	if _, err := s.cfg.Accounts.Record(r.Context(), e); err != nil {
		s.logger.Error("audit log write failed", "action", e.Action, "error", err)
		telemetry.RecordLocalAnswer(r.Context(), in.Tool, telemetry.LocalRefused)
		s.problem(w, http.StatusServiceUnavailable, errors.New("the audit log could not record this call; ask the server instead"))
		return
	}
	s.countLocal(r, in, org, repo)
	w.WriteHeader(http.StatusNoContent)
}

// countLocal adds a local call to the usage buckets and to
// mcp_local_answers_total. A failure loses only the bucket: the call is
// already in the audit log.
func (s *Server) countLocal(r *http.Request, in localUsageReport, org, repo string) {
	telemetry.RecordLocalAnswer(r.Context(), in.Tool, telemetry.LocalRecorded)
	b := usage.Bucket{Day: usage.Day(time.Now()), Org: org, Repo: repo, Tool: in.Tool, Client: in.Client, Calls: 1,
		ResponseTokens: in.ResponseTokens, DurationMs: in.DurationMs, MaxDurationMs: in.DurationMs}
	if in.Failed {
		b.Errors = 1
	}
	if err := s.st.AddUsage(r.Context(), []usage.Bucket{b}); err != nil {
		s.logger.Warn("local call not counted", "tool", in.Tool, "error", err)
	}
}

// localAnswerAction is the audit action of a call the bridge answered.
const localAnswerAction = "mcp.local_answer"

// localTools are the tools the bridge may answer on the developer's machine.
func localTools() []string {
	return slices.Concat(gittools.Tools, ghtools.Tools, coretools.Local, []string{asklocal.Tool, asklocal.DraftTool, fetchsource.Tool})
}

func (in localUsageReport) validate() error {
	switch {
	case !slices.Contains(localTools(), in.Tool):
		return fmt.Errorf("unknown local tool %q", in.Tool)
	case !clientNameRe.MatchString(in.Client):
		return errors.New("client must be a short lower-case name, e.g. claude-code")
	case in.Repo != "" && !validRepoName(in.Repo):
		return errors.New("repo must be a repository name such as owner/name")
	case in.DurationMs < 0 || in.DurationMs > maxLocalDurationMs:
		return errors.New("duration_ms out of range")
	case in.ResponseTokens < 0 || in.ResponseTokens > maxLocalTokens:
		return errors.New("response_tokens out of range")
	}
	return nil
}
