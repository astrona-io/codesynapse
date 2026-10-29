package ghtools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The tool names. They read the developer's GitHub sign-in, which only the
// bridge on their machine has.
const (
	ToolPRStatus = "gh_pr_status"
	ToolPRList   = "gh_pr_list"
	ToolRunFail  = "gh_run_failure"
	ToolRunList  = "gh_run_list"
)

// Tools lists them.
var Tools = []string{ToolPRStatus, ToolPRList, ToolRunFail, ToolRunList}

// PRStatusIn is gh_pr_status's input.
type PRStatusIn struct {
	// Number is the pull request; 0 is the one for the checked-out branch.
	Number int `json:"number,omitempty" jsonschema:"pull request number; omit for the checked-out branch's pull request"`
	// WaitSeconds blocks until the checks are done or one fails, at most this
	// long (0 answers now).
	WaitSeconds int `json:"wait_seconds,omitempty" jsonschema:"wait up to this many seconds (at most 300) for the checks to finish or one to fail, then answer; omit to answer now"`
}

// PRListIn is gh_pr_list's input.
type PRListIn struct {
	State string `json:"state,omitempty" jsonschema:"open (default), closed, merged or all"`
	Limit int    `json:"limit,omitempty" jsonschema:"how many to list, default 10, at most 30"`
}

// RunListIn is gh_run_list's input. The filters are applied here, after gh
// answers, so no text from the agent reaches gh's arguments.
type RunListIn struct {
	Workflow   string `json:"workflow,omitempty" jsonschema:"only runs of this workflow (its name, as shown in the list)"`
	Branch     string `json:"branch,omitempty" jsonschema:"only runs on this branch"`
	Conclusion string `json:"conclusion,omitempty" jsonschema:"only runs that ended so: success, failure, cancelled or skipped"`
	Limit      int    `json:"limit,omitempty" jsonschema:"how many to list, default 10, at most 30"`
}

// RunSummary is one line of a run list.
type RunSummary struct {
	ID         int64  `json:"id"`
	Workflow   string `json:"workflow"`
	Branch     string `json:"branch"`
	Commit     string `json:"commit"`
	Event      string `json:"event"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
	Created    string `json:"created"`
}

// RunListOut is gh_run_list's answer.
type RunListOut struct {
	Runs []RunSummary `json:"runs"`
	// Scanned is how many recent runs the filters looked through.
	Scanned int `json:"scanned"`
}

// Checks counts a pull request's checks and names the ones to look at.
type Checks struct {
	Pass    int `json:"pass"`
	Fail    int `json:"fail"`
	Pending int `json:"pending"`
	Skipped int `json:"skipped"`
	// Done is true when no check is still running.
	Done    bool     `json:"done"`
	Failing []string `json:"failing,omitempty"`
	Waiting []string `json:"waiting,omitempty"`
}

// PRStatus is where one pull request stands.
type PRStatus struct {
	Number         int    `json:"number"`
	Title          string `json:"title"`
	State          string `json:"state"` // OPEN, CLOSED, MERGED
	Draft          bool   `json:"draft,omitempty"`
	Head           string `json:"head"`
	Base           string `json:"base"`
	Mergeable      string `json:"mergeable"`                 // MERGEABLE, CONFLICTING, UNKNOWN
	MergeState     string `json:"merge_state,omitempty"`     // CLEAN, BLOCKED, BEHIND, DIRTY, …
	ReviewDecision string `json:"review_decision,omitempty"` // APPROVED, CHANGES_REQUESTED, REVIEW_REQUIRED
	Checks         Checks `json:"checks"`
	// WaitedSeconds and TimedOut describe a wait_seconds request: how long it
	// waited, and whether the checks were still running when it gave up.
	WaitedSeconds int  `json:"waited_seconds,omitempty"`
	TimedOut      bool `json:"timed_out,omitempty"`
}

// PRSummary is one line of a pull request list.
type PRSummary struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Head   string `json:"head"`
	State  string `json:"state"`
	Draft  bool   `json:"draft,omitempty"`
}

// PRListOut is gh_pr_list's answer.
type PRListOut struct {
	PullRequests []PRSummary `json:"pull_requests"`
}

// Run answers one of the tools for the checkout at dir. args is the tool's
// JSON arguments (unknown fields are refused); the result is the answer to
// encode.
func Run(ctx context.Context, dir, tool string, args json.RawMessage) (any, error) {
	decode := func(v any) error {
		if len(bytes.TrimSpace(args)) == 0 {
			return nil
		}
		dec := json.NewDecoder(bytes.NewReader(args))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		return nil
	}
	switch tool {
	case ToolPRStatus:
		var in PRStatusIn
		if err := decode(&in); err != nil {
			return nil, err
		}
		return Status(ctx, dir, in)
	case ToolPRList:
		var in PRListIn
		if err := decode(&in); err != nil {
			return nil, err
		}
		return List(ctx, dir, in)
	case ToolRunFail:
		var in RunFailureIn
		if err := decode(&in); err != nil {
			return nil, err
		}
		return Failure(ctx, dir, in)
	case ToolRunList:
		var in RunListIn
		if err := decode(&in); err != nil {
			return nil, err
		}
		return Runs(ctx, dir, in)
	}
	return nil, fmt.Errorf("%w: unknown tool %q", ErrInvalid, tool)
}

// MaxWaitSeconds is the longest gh_pr_status may wait for checks.
const MaxWaitSeconds = 300

// The poll interval, the grace and the clock are variables so tests run on a
// fake clock instead of depending on how fast gh answers.
var (
	pollInterval = 15 * time.Second
	// noChecksGrace is how long a wait lets checks appear: right after a push
	// none exist yet, and a repository without CI never gets any.
	noChecksGrace = 45 * time.Second
	now           = time.Now
	sleep         = func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
)

// Timeout is how long the bridge should let a call run: the usual bound plus
// the wait a gh_pr_status call asked for.
func Timeout(base time.Duration, tool string, args json.RawMessage) time.Duration {
	var in PRStatusIn
	if tool != ToolPRStatus || json.Unmarshal(args, &in) != nil {
		return base
	}
	return base + time.Duration(min(max(in.WaitSeconds, 0), MaxWaitSeconds))*time.Second
}

// Status answers gh_pr_status. With WaitSeconds it polls until the checks are
// done or one fails (so a fix can start while the rest still run), or the
// time is up.
func Status(ctx context.Context, dir string, in PRStatusIn) (PRStatus, error) {
	if in.Number < 0 || in.Number > MaxNumber || in.WaitSeconds < 0 || in.WaitSeconds > MaxWaitSeconds {
		return PRStatus{}, fmt.Errorf("%w: number must be a pull request number and wait_seconds between 0 and %d", ErrInvalid, MaxWaitSeconds)
	}
	start := now()
	deadline := start.Add(time.Duration(in.WaitSeconds) * time.Second)
	for {
		st, err := status(ctx, dir, in.Number)
		if err != nil || in.WaitSeconds == 0 {
			return st, err
		}
		c := st.Checks
		total := c.Pass + c.Fail + c.Pending + c.Skipped
		finished := st.State != "OPEN" || c.Fail > 0 || (c.Done && total > 0) ||
			(total == 0 && now().Sub(start) >= noChecksGrace)
		if finished || !now().Before(deadline) {
			st.WaitedSeconds = int(now().Sub(start).Round(time.Second) / time.Second)
			st.TimedOut = !finished
			return st, nil
		}
		if err := sleep(ctx, min(pollInterval, deadline.Sub(now()))); err != nil {
			return st, err
		}
	}
}

func status(ctx context.Context, dir string, number int) (PRStatus, error) {
	in := PRStatusIn{Number: number}
	args := []string{"pr", "view"}
	if in.Number > 0 {
		args = append(args, strconv.Itoa(in.Number))
	}
	args = append(args, "--json", "number,title,state,isDraft,headRefName,baseRefName,mergeable,mergeStateStatus,reviewDecision,statusCheckRollup")
	raw, err := gh(ctx, dir, args...)
	if err != nil {
		return PRStatus{}, err
	}
	var v struct {
		Number           int          `json:"number"`
		Title            string       `json:"title"`
		State            string       `json:"state"`
		IsDraft          bool         `json:"isDraft"`
		HeadRefName      string       `json:"headRefName"`
		BaseRefName      string       `json:"baseRefName"`
		Mergeable        string       `json:"mergeable"`
		MergeStateStatus string       `json:"mergeStateStatus"`
		ReviewDecision   string       `json:"reviewDecision"`
		Rollup           []rollupItem `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return PRStatus{}, fmt.Errorf("%w: unreadable answer from gh", ErrUnavailable)
	}
	return PRStatus{
		Number: v.Number, Title: safeText(v.Title, titleLimit), State: v.State, Draft: v.IsDraft,
		Head: safeText(v.HeadRefName, nameLimit), Base: safeText(v.BaseRefName, nameLimit),
		Mergeable: v.Mergeable, MergeState: v.MergeStateStatus, ReviewDecision: v.ReviewDecision,
		Checks: countChecks(v.Rollup),
	}, nil
}

// rollupItem is a check run or a commit status in gh's rollup.
type rollupItem struct {
	Type       string `json:"__typename"`
	Name       string `json:"name"`       // CheckRun
	Status     string `json:"status"`     // CheckRun: COMPLETED, IN_PROGRESS, QUEUED, …
	Conclusion string `json:"conclusion"` // CheckRun
	Context    string `json:"context"`    // StatusContext
	State      string `json:"state"`      // StatusContext: SUCCESS, PENDING, FAILURE, …
}

// countChecks sorts a rollup into pass, fail, pending and skipped, and names
// the failing and waiting ones (each name once, at most MaxNames).
func countChecks(items []rollupItem) Checks {
	var c Checks
	add := func(list *[]string, name string) {
		name = safeText(name, nameLimit)
		if len(*list) < MaxNames && !slices.Contains(*list, name) {
			*list = append(*list, name)
		}
	}
	for _, it := range items {
		name := it.Name
		verdict := ""
		if it.Type == "StatusContext" {
			name = it.Context
			switch it.State {
			case "SUCCESS":
				verdict = "pass"
			case "PENDING", "EXPECTED":
				verdict = "pending"
			default:
				verdict = "fail"
			}
		} else {
			switch {
			case it.Status != "COMPLETED":
				verdict = "pending"
			case it.Conclusion == "SUCCESS":
				verdict = "pass"
			case it.Conclusion == "NEUTRAL" || it.Conclusion == "SKIPPED":
				verdict = "skipped"
			default: // failure, cancelled, timed out, action required, …
				verdict = "fail"
			}
		}
		switch verdict {
		case "pass":
			c.Pass++
		case "skipped":
			c.Skipped++
		case "pending":
			c.Pending++
			add(&c.Waiting, name)
		default:
			c.Fail++
			add(&c.Failing, name)
		}
	}
	c.Done = c.Pending == 0
	return c
}

// List answers gh_pr_list.
func List(ctx context.Context, dir string, in PRListIn) (PRListOut, error) {
	state := in.State
	if state == "" {
		state = "open"
	}
	if !slices.Contains([]string{"open", "closed", "merged", "all"}, state) {
		return PRListOut{}, fmt.Errorf("%w: state must be open, closed, merged or all", ErrInvalid)
	}
	limit := in.Limit
	switch {
	case limit < 0:
		return PRListOut{}, fmt.Errorf("%w: limit must be positive", ErrInvalid)
	case limit == 0:
		limit = defaultList
	}
	limit = min(limit, MaxList)
	raw, err := gh(ctx, dir, "pr", "list", "--state", state, "--limit", strconv.Itoa(limit), "--json", "number,title,headRefName,state,isDraft")
	if err != nil {
		return PRListOut{}, err
	}
	var v []struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		HeadRefName string `json:"headRefName"`
		State       string `json:"state"`
		IsDraft     bool   `json:"isDraft"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return PRListOut{}, fmt.Errorf("%w: unreadable answer from gh", ErrUnavailable)
	}
	out := PRListOut{PullRequests: make([]PRSummary, 0, len(v))}
	for _, p := range v {
		out.PullRequests = append(out.PullRequests, PRSummary{
			Number: p.Number, Title: safeText(p.Title, titleLimit), Head: safeText(p.HeadRefName, nameLimit), State: p.State, Draft: p.IsDraft,
		})
	}
	return out, nil
}

// runScan is how many recent runs gh_run_list asks for before filtering.
const runScan = 200

// Runs answers gh_run_list: recent workflow runs, newest first, one line
// each, filtered here by workflow, branch and conclusion.
func Runs(ctx context.Context, dir string, in RunListIn) (RunListOut, error) {
	if in.Conclusion != "" && !slices.Contains([]string{"success", "failure", "cancelled", "skipped"}, in.Conclusion) {
		return RunListOut{}, fmt.Errorf("%w: conclusion must be success, failure, cancelled or skipped", ErrInvalid)
	}
	limit := in.Limit
	switch {
	case limit < 0:
		return RunListOut{}, fmt.Errorf("%w: limit must be positive", ErrInvalid)
	case limit == 0:
		limit = defaultList
	}
	limit = min(limit, MaxList)
	raw, err := gh(ctx, dir, "run", "list", "--limit", strconv.Itoa(runScan), "--json", "databaseId,workflowName,headBranch,headSha,event,status,conclusion,createdAt")
	if err != nil {
		return RunListOut{}, err
	}
	var v []struct {
		DatabaseID   int64  `json:"databaseId"`
		WorkflowName string `json:"workflowName"`
		HeadBranch   string `json:"headBranch"`
		HeadSha      string `json:"headSha"`
		Event        string `json:"event"`
		Status       string `json:"status"`
		Conclusion   string `json:"conclusion"`
		CreatedAt    string `json:"createdAt"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return RunListOut{}, fmt.Errorf("%w: unreadable answer from gh", ErrUnavailable)
	}
	out := RunListOut{Runs: []RunSummary{}, Scanned: len(v)}
	for _, r := range v {
		if (in.Workflow != "" && !strings.EqualFold(r.WorkflowName, in.Workflow)) || (in.Branch != "" && r.HeadBranch != in.Branch) ||
			(in.Conclusion != "" && !strings.EqualFold(r.Conclusion, in.Conclusion)) {
			continue
		}
		out.Runs = append(out.Runs, RunSummary{
			ID: r.DatabaseID, Workflow: safeText(r.WorkflowName, titleLimit), Branch: safeText(r.HeadBranch, nameLimit), Commit: shortSha(r.HeadSha),
			Event: safeText(r.Event, nameLimit), Status: strings.ToLower(r.Status), Conclusion: strings.ToLower(r.Conclusion), Created: r.CreatedAt,
		})
		if len(out.Runs) == limit {
			break
		}
	}
	return out, nil
}

// shortSha keeps the first seven hex digits of a commit.
func shortSha(s string) string {
	if len(s) < 7 {
		return ""
	}
	for _, c := range s[:7] {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return ""
		}
	}
	return s[:7]
}
