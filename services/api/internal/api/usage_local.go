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
	// Source is fetch_source's measurement (docs/plans/TEXT_OPTIMIZATION_PLAN.md,
	// pillar D): how the page was answered and its sizes, never its URL.
	Source *sourceReport `json:"source,omitempty"`
}

// sourceReport is one fetch_source answer: the result
// (telemetry.SourceResults), the estimated tokens of the page as served, of
// its cleaned text and of the text returned, and the microseconds reducing
// it took on the developer's machine.
type sourceReport struct {
	Result          string `json:"result"`
	RawTokens       int    `json:"raw_tokens"`
	CleanedTokens   int    `json:"cleaned_tokens"`
	ExtractedTokens int    `json:"extracted_tokens"`
	ReduceUs        int    `json:"reduce_us"`
}

const (
	// maxSourceTokens is fetchsource.MaxBody in tokens, with room.
	maxSourceTokens = 1_000_000
	maxSourceUs     = 60 * 1_000_000
)

func (sr *sourceReport) validate() error {
	switch {
	case !slices.Contains(telemetry.SourceResults, sr.Result):
		return fmt.Errorf("unknown source result %q", sr.Result)
	case sr.RawTokens < 0 || sr.RawTokens > maxSourceTokens, sr.CleanedTokens < 0 || sr.CleanedTokens > maxSourceTokens,
		sr.ExtractedTokens < 0 || sr.ExtractedTokens > maxSourceTokens:
		return errors.New("source tokens out of range")
	case sr.ReduceUs < 0 || sr.ReduceUs > maxSourceUs:
		return errors.New("source reduce_us out of range")
	}
	return nil
}

// returned reports whether a page was returned (not refused, not failed).
func (sr *sourceReport) returned() bool {
	return sr.Result == telemetry.SourceHit || sr.Result == telemetry.SourceRevalidated || sr.Result == telemetry.SourceMiss
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
	buckets := append([]usage.Bucket{b}, s.countSource(r, in, org)...)
	if err := s.st.AddUsage(r.Context(), buckets); err != nil {
		s.logger.Warn("local call not counted", "tool", in.Tool, "error", err)
	}
}

// countSource records fetch_source's measurement in the source instruments
// and returns its daily buckets: pages returned, cache hits, raw tokens and
// tokens saved (raw minus returned, per page). They carry the organisation
// and no repository.
func (s *Server) countSource(r *http.Request, in localUsageReport, org string) []usage.Bucket {
	sr := in.Source
	if sr == nil {
		return nil
	}
	telemetry.RecordSourceFetch(r.Context(), in.Tool, telemetry.SourceFetch{Result: sr.Result, RawTokens: sr.RawTokens,
		CleanedTokens: sr.CleanedTokens, ExtractedTokens: sr.ExtractedTokens, Tier: telemetry.TierCPU,
		Reduce: time.Duration(sr.ReduceUs) * time.Microsecond})
	if !sr.returned() {
		return nil
	}
	day := usage.Day(time.Now())
	counter := func(tool string, n int) usage.Bucket {
		return usage.Bucket{Day: day, Org: org, Tool: tool, Client: in.Client, Calls: n}
	}
	out := []usage.Bucket{counter(usage.SourceFetches, 1), counter(usage.SourceRaw, sr.RawTokens),
		counter(usage.SourceSaved, max(0, sr.RawTokens-sr.ExtractedTokens))}
	if sr.Result != telemetry.SourceMiss {
		out = append(out, counter(usage.SourceHits, 1))
	}
	return out
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
	case in.Source != nil && in.Tool != fetchsource.Tool:
		return errors.New("source is reported for fetch_source only")
	case in.Source != nil:
		return in.Source.validate()
	}
	return nil
}
