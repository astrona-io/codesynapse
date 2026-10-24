package api

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/toolsets"
	"github.com/astrona-io/codesynapse/internal/usage"
)

// Retrieval usefulness (docs/PLAN.md, agent workflow roadmap step 13): the
// Claude Code plugin's hooks count, per tool, the file line ranges answers
// gave and how many of them the agent then read, and report the counts
// here. Only tool names and counts arrive: no paths, ranges, content or
// session IDs. They join the daily usage buckets of the caller's
// organisation and show on the Statistics page as a read rate per tool.

const (
	maxRetrievalTools = 64
	// maxRetrievalRanges bounds one report's count per tool; a session
	// reports at most every Stop, so a larger number is not a real one.
	maxRetrievalRanges = 10_000
)

var clientNameRe = regexp.MustCompile(`^[a-z0-9._-]{1,64}$`)

type retrievalCounts struct {
	Returned int `json:"returned"`
	Read     int `json:"read"`
}

// workflowCounts say how sessions began (see usage.FirstCodeSynapse): counts
// since the last report, nothing about what was read or searched.
type workflowCounts struct {
	FirstCodeSynapse int `json:"first_codesynapse"`
	FirstNative      int `json:"first_native"`
	ReadsTurnedBack  int `json:"reads_turned_back"`
	// FilesEdited and FilesEditedFound: files the session changed, and of
	// them those a CodeSynapse answer had pointed to (usage.FilesEdited).
	FilesEdited      int `json:"files_edited"`
	FilesEditedFound int `json:"files_edited_found"`
}

type retrievalReport struct {
	Edits    *usage.EditCounts          `json:"edits,omitempty"`
	Tokens   *usage.TokenCounts         `json:"tokens,omitempty"`
	Client   string                     `json:"client"`
	Tools    map[string]retrievalCounts `json:"tools"`
	Workflow *workflowCounts            `json:"workflow,omitempty"`
}

func (s *Server) retrievalRoutes() {
	s.route("POST /api/v1/repos/{repo}/usage/retrieval", repoRole(accounts.Viewer), s.handleRetrievalReport)
}

func (s *Server) handleRetrievalReport(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	if _, err := s.st.Node(r.Context(), model.NodeID(repo, model.KindRepository, repo)); err != nil {
		s.problem(w, http.StatusNotFound, err)
		return
	}
	var in retrievalReport
	if err := decodeJSON(w, r, &in); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	if err := in.validate(); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	org, err := s.repoOrg(r, repo)
	if err != nil {
		s.accountError(w, err)
		return
	}
	day := usage.Day(time.Now())
	buckets := make([]usage.Bucket, 0, len(in.Tools))
	if in.Edits != nil {
		for tool, n := range in.Edits.Counters() {
			if n > 0 {
				buckets = append(buckets, usage.Bucket{Day: day, Org: org, Repo: repo, Tool: tool, Client: in.Client, Calls: n})
			}
		}
	}
	if in.Tokens != nil {
		for tool, n := range in.Tokens.Counters() {
			if n > 0 {
				buckets = append(buckets, usage.Bucket{Day: day, Org: org, Repo: repo, Tool: tool, Client: in.Client, Calls: n})
			}
		}
	}
	for tool, c := range in.Tools {
		if c.Returned == 0 && c.Read == 0 {
			continue
		}
		buckets = append(buckets, usage.Bucket{Day: day, Org: org, Repo: repo, Tool: tool, Client: in.Client, RangesReturned: c.Returned, RangesRead: c.Read})
	}
	if wf := in.Workflow; wf != nil {
		for tool, n := range map[string]int{usage.FirstCodeSynapse: wf.FirstCodeSynapse, usage.FirstNative: wf.FirstNative, usage.ReadTurnedBack: wf.ReadsTurnedBack,
			usage.FilesEdited: wf.FilesEdited, usage.FilesEditedFound: wf.FilesEditedFound} {
			if n > 0 {
				buckets = append(buckets, usage.Bucket{Day: day, Org: org, Repo: repo, Tool: tool, Client: in.Client, Calls: n})
			}
		}
	}
	if len(buckets) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.st.AddUsage(r.Context(), buckets); err != nil {
		s.problem(w, 500, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (in retrievalReport) validate() error {
	if !clientNameRe.MatchString(in.Client) {
		return errors.New("client must be a short lower-case name, e.g. claude-code")
	}
	if in.Tokens != nil {
		if err := in.Tokens.Validate(); err != nil {
			return err
		}
	}
	if in.Edits != nil {
		if err := in.Edits.Validate(); err != nil {
			return err
		}
	}
	if len(in.Tools) > maxRetrievalTools || (len(in.Tools) == 0 && in.Workflow == nil && in.Tokens == nil && in.Edits == nil) {
		return fmt.Errorf("report up to %d tools, or the workflow counts", maxRetrievalTools)
	}
	if wf := in.Workflow; wf != nil {
		for _, n := range []int{wf.FirstCodeSynapse, wf.FirstNative, wf.ReadsTurnedBack, wf.FilesEdited, wf.FilesEditedFound} {
			if n < 0 || n > maxRetrievalRanges {
				return fmt.Errorf("workflow counts must be between 0 and %d", maxRetrievalRanges)
			}
		}
	}
	for tool, c := range in.Tools {
		if !toolsets.Known(tool) {
			return fmt.Errorf("unknown tool %q", tool)
		}
		// Read may exceed returned: a range given before the last report
		// can be read after it.
		if c.Returned < 0 || c.Read < 0 || c.Returned > maxRetrievalRanges || c.Read > maxRetrievalRanges {
			return fmt.Errorf("%s: counts must be between 0 and %d", tool, maxRetrievalRanges)
		}
	}
	return nil
}
