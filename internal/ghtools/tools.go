package ghtools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"
)

// The tool names. They read the developer's GitHub sign-in, which only the
// bridge on their machine has.
const (
	ToolPRStatus = "gh_pr_status"
	ToolPRList   = "gh_pr_list"
	ToolRunFail  = "gh_run_failure"
)

// Tools lists them.
var Tools = []string{ToolPRStatus, ToolPRList, ToolRunFail}

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
	}
	return nil, fmt.Errorf("%w: unknown tool %q", ErrInvalid, tool)
}

// MaxWaitSeconds is the longest gh_pr_status may wait for checks.
const MaxWaitSeconds = 300

// The poll interval and the grace are variables so tests do not sleep.
var (
	pollInterval = 15 * time.Second
	// noChecksGrace is how long a wait lets checks appear: right after a push
	// none exist yet, and a repository without CI never gets any.
	noChecksGrace = 45 * time.Second
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
	start := time.Now()
	deadline := start.Add(time.Duration(in.WaitSeconds) * time.Second)
	for {
		st, err := status(ctx, dir, in.Number)
		if err != nil || in.WaitSeconds == 0 {
			return st, err
		}
		c := st.Checks
		total := c.Pass + c.Fail + c.Pending + c.Skipped
		finished := st.State != "OPEN" || c.Fail > 0 || (c.Done && total > 0) ||
			(total == 0 && time.Since(start) >= noChecksGrace)
		if finished || !time.Now().Before(deadline) {
			st.WaitedSeconds = int(time.Since(start).Round(time.Second) / time.Second)
			st.TimedOut = !finished
			return st, nil
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(min(pollInterval, time.Until(deadline))):
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
