package ghtools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Limits on a run failure answer.
const (
	MaxFailedJobs = 5
	MaxErrorLines = 12
	lineLimit     = 200
	tailLines     = 8
	maxRuns       = 10
)

// RunFailureIn is gh_run_failure's input.
type RunFailureIn struct {
	// Run is a workflow run ID; 0 picks the latest failed run of the branch.
	Run int `json:"run,omitempty" jsonschema:"workflow run ID; omit for the latest failed run of the pull request's branch"`
	// Number is the pull request whose branch to look at; 0 is the checked-out branch's.
	Number int `json:"number,omitempty" jsonschema:"pull request number, when run is omitted; omit for the checked-out branch"`
}

// FailedJob is one failed job of a run.
type FailedJob struct {
	Job   string   `json:"job"`
	Steps []string `json:"failed_steps,omitempty"`
	// Lines are the error lines of the job's log, or its last lines when none
	// looks like an error.
	Lines []string `json:"lines"`
}

// RunFailure is why a workflow run failed.
type RunFailure struct {
	Run      int         `json:"run"`
	Workflow string      `json:"workflow"`
	Failed   []FailedJob `json:"failed_jobs"`
	// Truncated says the log was longer than gh_run_failure reads.
	Truncated bool `json:"truncated,omitempty"`
}

// branchRe is what a branch name may contain here: no leading dash, so it can
// never read as an option.
var branchRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]{0,199}$`)

// Failure answers gh_run_failure.
func Failure(ctx context.Context, dir string, in RunFailureIn) (RunFailure, error) {
	if in.Run < 0 || in.Run > MaxNumber*1000 || in.Number < 0 || in.Number > MaxNumber {
		return RunFailure{}, fmt.Errorf("%w: run and number must be positive IDs", ErrInvalid)
	}
	out := RunFailure{Run: in.Run, Failed: make([]FailedJob, 0, MaxFailedJobs)} // byJob points into it: it must never reallocate
	if in.Run == 0 {
		id, name, err := latestFailedRun(ctx, dir, in.Number)
		if err != nil {
			return out, err
		}
		out.Run, out.Workflow = id, name
	}
	raw, err := gh(ctx, dir, "run", "view", strconv.Itoa(out.Run), "--json", "workflowName,jobs")
	if err != nil {
		return out, err
	}
	var v struct {
		WorkflowName string `json:"workflowName"`
		Jobs         []struct {
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
			Steps      []struct {
				Name       string `json:"name"`
				Conclusion string `json:"conclusion"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return out, fmt.Errorf("%w: unreadable answer from gh", ErrUnavailable)
	}
	out.Workflow = safeText(v.WorkflowName, nameLimit)
	byJob := map[string]*FailedJob{}
	for _, j := range v.Jobs {
		if j.Conclusion != "failure" && j.Conclusion != "timed_out" && j.Conclusion != "cancelled" {
			continue
		}
		if len(out.Failed) == MaxFailedJobs {
			break
		}
		fj := FailedJob{Job: safeText(j.Name, nameLimit)}
		for _, s := range j.Steps {
			if s.Conclusion == "failure" || s.Conclusion == "timed_out" {
				fj.Steps = append(fj.Steps, safeText(s.Name, nameLimit))
			}
		}
		out.Failed = append(out.Failed, fj)
		byJob[j.Name] = &out.Failed[len(out.Failed)-1]
	}
	if len(out.Failed) == 0 {
		return out, fmt.Errorf("%w: run %d has no failed job", ErrUnavailable, out.Run)
	}
	log, truncated, err := ghLog(ctx, dir, "run", "view", strconv.Itoa(out.Run), "--log-failed")
	if err != nil {
		return out, err
	}
	out.Truncated = truncated
	perJob := map[string][]string{}
	for _, line := range strings.Split(string(log), "\n") {
		job, text, ok := splitLogLine(line)
		if ok && byJob[job] != nil {
			perJob[job] = append(perJob[job], text)
		}
	}
	for name, fj := range byJob {
		fj.Lines = errorLines(perJob[name])
	}
	return out, nil
}

// latestFailedRun finds the newest failed run on the branch of a pull request
// (the checked-out one when number is 0).
func latestFailedRun(ctx context.Context, dir string, number int) (int, string, error) {
	args := []string{"pr", "view"}
	if number > 0 {
		args = append(args, strconv.Itoa(number))
	}
	raw, err := gh(ctx, dir, append(args, "--json", "headRefName")...)
	if err != nil {
		return 0, "", err
	}
	var pr struct {
		Head string `json:"headRefName"`
	}
	if json.Unmarshal(raw, &pr) != nil || !branchRe.MatchString(pr.Head) {
		return 0, "", fmt.Errorf("%w: no usable branch for the pull request", ErrUnavailable)
	}
	raw, err = gh(ctx, dir, "run", "list", "--branch", pr.Head, "--limit", strconv.Itoa(maxRuns), "--json", "databaseId,workflowName,conclusion")
	if err != nil {
		return 0, "", err
	}
	var runs []struct {
		ID         int    `json:"databaseId"`
		Workflow   string `json:"workflowName"`
		Conclusion string `json:"conclusion"`
	}
	if err := json.Unmarshal(raw, &runs); err != nil {
		return 0, "", fmt.Errorf("%w: unreadable answer from gh", ErrUnavailable)
	}
	for _, r := range runs {
		if r.Conclusion == "failure" || r.Conclusion == "timed_out" {
			return r.ID, safeText(r.Workflow, nameLimit), nil
		}
	}
	return 0, "", fmt.Errorf("%w: no failed run among the last %d on this branch", ErrUnavailable, maxRuns)
}

// splitLogLine splits a "job<TAB>step<TAB>timestamp message" line of
// gh run view --log-failed into the job and the message.
func splitLogLine(line string) (job, text string, ok bool) {
	parts := strings.SplitN(line, "\t", 3)
	if len(parts) != 3 {
		return "", "", false
	}
	msg := parts[2]
	if ts, rest, found := strings.Cut(msg, " "); found && len(ts) > 19 && ts[4] == '-' && strings.HasSuffix(ts, "Z") {
		msg = rest // the timestamp GitHub puts in front of every line
	}
	return parts[0], ansi.ReplaceAllString(msg, ""), true
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// errorLike matches the lines CI output uses for a failure: Go and other test
// runners, compilers and linters (file:line:col), panics, npm, and the
// workflow's own ##[error] annotations.
var errorLike = regexp.MustCompile(`(?i)^(\s*--- FAIL|FAIL\b|panic:|fatal( error)?:|error[:\[]|##\[error\]|npm ERR!|not ok\b|\s*E\s{2,}|.*\bFAILED\b|.*\.[a-z]{1,4}:\d+(:\d+)?:\s*\S)`)

// errorLines picks the lines that say what went wrong, each once and in
// order; with none, the last few lines of the log.
func errorLines(lines []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || !errorLike.MatchString(l) {
			continue
		}
		l = safeText(l, lineLimit)
		if seen[l] {
			continue
		}
		seen[l] = true
		if out = append(out, l); len(out) == MaxErrorLines {
			return out
		}
	}
	if len(out) > 0 {
		return out
	}
	for i := len(lines) - 1; i >= 0 && len(out) < tailLines; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			out = append([]string{safeText(l, lineLimit)}, out...)
		}
	}
	return out
}
