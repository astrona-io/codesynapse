package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/astrona-io/codesynapse/internal/ghtools"
	"github.com/astrona-io/codesynapse/internal/gittools"
	"github.com/astrona-io/codesynapse/internal/usage"
)

// Calls the Claude Code bridge answers on the developer's machine (the git
// tools read a checkout the server cannot see) are reported here, so they are
// counted on Statistics and appear in the audit log (the HTTP middleware
// records every changing request) like the calls the MCP server answers. The
// report carries the tool name, the time it took and the size of the answer,
// nothing about the checkout.

const (
	maxLocalDurationMs = 10 * 60 * 1000
	maxLocalTokens     = 1_000_000
)

type localUsageReport struct {
	Tool           string `json:"tool"`
	Client         string `json:"client"`
	DurationMs     int    `json:"duration_ms"`
	ResponseTokens int    `json:"response_tokens"`
	Failed         bool   `json:"failed"`
}

func (s *Server) usageLocalRoutes() {
	s.route("POST /api/v1/usage/local", anyone, s.handleLocalUsage)
}

func (s *Server) handleLocalUsage(w http.ResponseWriter, r *http.Request) {
	var in localUsageReport
	if err := decodeJSON(w, r, &in); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	if err := in.validate(); err != nil {
		s.problem(w, http.StatusBadRequest, err)
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
	b := usage.Bucket{Day: usage.Day(time.Now()), Org: org, Tool: in.Tool, Client: in.Client, Calls: 1,
		ResponseTokens: in.ResponseTokens, DurationMs: in.DurationMs, MaxDurationMs: in.DurationMs}
	if in.Failed {
		b.Errors = 1
	}
	if err := s.st.AddUsage(r.Context(), []usage.Bucket{b}); err != nil {
		s.problem(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (in localUsageReport) validate() error {
	switch {
	case !slices.Contains(gittools.Tools, in.Tool) && !slices.Contains(ghtools.Tools, in.Tool):
		return fmt.Errorf("unknown local tool %q", in.Tool)
	case !clientNameRe.MatchString(in.Client):
		return errors.New("client must be a short lower-case name, e.g. claude-code")
	case in.DurationMs < 0 || in.DurationMs > maxLocalDurationMs:
		return errors.New("duration_ms out of range")
	case in.ResponseTokens < 0 || in.ResponseTokens > maxLocalTokens:
		return errors.New("response_tokens out of range")
	}
	return nil
}
