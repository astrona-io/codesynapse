// Package taskeval measures whether the agent tools point at the files a
// change ends up touching (docs/PLAN.md, agent workflow roadmap step 1).
//
// Each commit in base..head is a task: its subject is what an agent would be
// asked, and the files it modified or deleted that existed at base are the
// ground truth. The graph is built once at base, so no answer can draw on the
// change it is asked about. Files a commit added cannot be found and are
// only counted. No model is involved, so a run is cheap and repeatable.
package taskeval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"

	"github.com/astrona-io/codesynapse/internal/agentctx"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/query"
	"github.com/astrona-io/codesynapse/internal/store"
)

// Task is one commit turned into a question with known answers.
type Task struct {
	Commit  string   `json:"commit"`
	Subject string   `json:"subject"`
	Truth   []string `json:"truth"`
	// Added counts files the commit created; no answer can name them.
	Added int `json:"added,omitempty"`
}

// Options select the tasks.
type Options struct {
	// MaxFiles skips sweeping commits (renames, formatting, generated
	// updates) that touch more files than this; 0 means 25.
	MaxFiles int
	// Skip matches subjects that are no task for an agent; nil means
	// dependency updates and reverts.
	Skip *regexp.Regexp
}

var defaultSkip = regexp.MustCompile(`^(chore\(deps\)|Revert |Merge )`)

// Commits lists base..head without merges, oldest first.
func Commits(ctx context.Context, dir, base, head string) ([]Commit, error) {
	for _, rev := range []string{base, head} {
		if err := gitx.ValidateRev(rev); err != nil {
			return nil, err
		}
	}
	out, err := gitx.Run(ctx, dir, "log", "--no-merges", "--no-renames", "--reverse", "--format=%x1e%H%x1f%s", "--name-status", base+".."+head, "--")
	if err != nil {
		return nil, err
	}
	return parseLog(out), nil
}

// Commit is a commit's subject and changed files.
type Commit struct {
	Hash, Subject string
	// Changes are git's status letter (A, M, D, T) and path.
	Changes [][2]string
}

func parseLog(out []byte) []Commit {
	var commits []Commit
	for rec := range bytes.SplitSeq(out, []byte{0x1e}) {
		lines := strings.Split(strings.TrimSpace(string(rec)), "\n")
		head, _, ok := strings.Cut(lines[0], "\x1f")
		if !ok {
			continue
		}
		c := Commit{Hash: head, Subject: strings.TrimSpace(strings.SplitN(lines[0], "\x1f", 2)[1])}
		for _, l := range lines[1:] {
			status, path, ok := strings.Cut(strings.TrimSpace(l), "\t")
			if ok && status != "" && path != "" {
				c.Changes = append(c.Changes, [2]string{status[:1], path})
			}
		}
		commits = append(commits, c)
	}
	return commits
}

// Tasks keeps the commits that make a fair task: a real subject, not too
// many files, and at least one changed file indexed at base.
func Tasks(commits []Commit, indexed map[string]bool, opts Options) []Task {
	maxFiles := opts.MaxFiles
	if maxFiles <= 0 {
		maxFiles = 25
	}
	skip := opts.Skip
	if skip == nil {
		skip = defaultSkip
	}
	var tasks []Task
	for _, c := range commits {
		if c.Subject == "" || skip.MatchString(c.Subject) || len(c.Changes) > maxFiles {
			continue
		}
		t := Task{Commit: c.Hash, Subject: c.Subject}
		for _, ch := range c.Changes {
			switch {
			case ch[0] == "A":
				t.Added++
			case indexed[ch[1]] && !slices.Contains(t.Truth, ch[1]):
				t.Truth = append(t.Truth, ch[1])
			}
		}
		if len(t.Truth) > 0 {
			tasks = append(tasks, t)
		}
	}
	return tasks
}

// IndexedFiles are the files any node of the graph lives in: Go files,
// Markdown documents, YAML and templates alike.
func IndexedFiles(g *model.Graph) map[string]bool {
	files := map[string]bool{}
	for _, n := range g.Nodes() {
		if n.File != "" {
			files[n.File] = true
		}
	}
	return files
}

// Tool names in a report.
const (
	ToolMinimalContext = "minimal_context"
	ToolSearch         = "search"
)

// Score is one tool's result on one task.
type Score struct {
	Recall5  float64 `json:"recall_at_5"`
	Recall10 float64 `json:"recall_at_10"`
	Tokens   int     `json:"tokens"`
	// Files are the distinct files the answer named, in its order.
	Files []string `json:"files,omitempty"`
	Error string   `json:"error,omitempty"`
}

// Summary averages a tool's scores over all tasks.
type Summary struct {
	Recall5  float64 `json:"recall_at_5"`
	Recall10 float64 `json:"recall_at_10"`
	// Hit10 is the share of tasks with at least one right file in ten.
	Hit10  float64 `json:"hit_at_10"`
	Tokens float64 `json:"tokens"`
	Errors int     `json:"errors"`
}

// Result is one task with each tool's score.
type Result struct {
	Task
	Scores map[string]Score `json:"scores"`
}

// Report is a whole run.
type Report struct {
	Repo    string             `json:"repo"`
	Base    string             `json:"base"`
	Head    string             `json:"head"`
	Tasks   int                `json:"tasks"`
	Summary map[string]Summary `json:"summary"`
	Results []Result           `json:"results,omitempty"`
}

// MinimalContextBudget is the token budget minimal_context gets, its default.
const MinimalContextBudget = 1500

// Run asks both tools every task against the graph in r.
func Run(ctx context.Context, r store.Reader, repo string, tasks []Task) Report {
	svc := agentctx.Service{R: r}
	rep := Report{Repo: repo, Tasks: len(tasks), Summary: map[string]Summary{}}
	for _, t := range tasks {
		res := Result{Task: t, Scores: map[string]Score{}}
		mc, err := svc.MinimalContext(ctx, repo, t.Subject, nil, MinimalContextBudget)
		res.Scores[ToolMinimalContext] = score(t.Truth, mc, err, func() []string {
			files := make([]string, 0, len(mc.Items))
			for _, it := range mc.Items {
				files = append(files, fileOf(it.Read))
			}
			return files
		})
		res.Scores[ToolSearch] = searchKeywords(ctx, r, repo, t.Truth, mc.Keywords, err)
		rep.Results = append(rep.Results, res)
	}
	for _, tool := range []string{ToolMinimalContext, ToolSearch} {
		rep.Summary[tool] = summarize(rep.Results, tool)
	}
	return rep
}

// SearchKeywords is how many of minimal_context's keywords the search
// stand-in looks up, one call each, as an agent would.
const SearchKeywords = 5

// searchKeywords scores search the way an agent uses it: it matches names,
// so a sentence finds nothing; the agent looks up the task's identifiers one
// by one. Results are interleaved by rank, and every call's answer counts.
func searchKeywords(ctx context.Context, r store.Reader, repo string, truth, keywords []string, mcErr error) Score {
	if mcErr != nil {
		return Score{Error: "no keywords: " + mcErr.Error()}
	}
	var answers [][]query.Ref
	for _, kw := range keywords[:min(SearchKeywords, len(keywords))] {
		nodes, err := r.Search(ctx, repo, kw, nil, 10)
		if err != nil {
			return Score{Error: err.Error()}
		}
		refs := make([]query.Ref, 0, len(nodes))
		for _, n := range nodes {
			refs = append(refs, query.RefOf(n))
		}
		answers = append(answers, refs)
	}
	return score(truth, answers, nil, func() []string {
		var files []string
		for rank := range 10 {
			for _, refs := range answers {
				if rank < len(refs) {
					files = append(files, fileOf(refs[rank].Location))
				}
			}
		}
		return files
	})
}

// fileOf drops a location's line range ("a/b.go:10-20" -> "a/b.go").
func fileOf(loc string) string {
	if i := strings.LastIndexByte(loc, ':'); i > 0 && strings.Trim(loc[i+1:], "0123456789-") == "" {
		return loc[:i]
	}
	return loc
}

// score rates an answer: recall over the distinct files it names, in order,
// and its size the way phase 4f counted it (serialized bytes / 4).
func score(truth []string, answer any, err error, files func() []string) Score {
	if err != nil {
		return Score{Error: err.Error()}
	}
	var s Score
	if b, mErr := json.Marshal(answer); mErr == nil {
		s.Tokens = int(math.Ceil(float64(len(b)) / 4))
	}
	for _, f := range files() {
		if f != "" && !slices.Contains(s.Files, f) {
			s.Files = append(s.Files, f)
		}
	}
	s.Recall5, s.Recall10 = recall(truth, s.Files, 5), recall(truth, s.Files, 10)
	return s
}

func recall(truth, files []string, k int) float64 {
	found := 0
	for _, f := range files[:min(k, len(files))] {
		if slices.Contains(truth, f) {
			found++
		}
	}
	return float64(found) / float64(len(truth))
}

func summarize(results []Result, tool string) Summary {
	var s Summary
	if len(results) == 0 {
		return s
	}
	for _, r := range results {
		sc := r.Scores[tool]
		if sc.Error != "" {
			s.Errors++
		}
		s.Recall5 += sc.Recall5
		s.Recall10 += sc.Recall10
		s.Tokens += float64(sc.Tokens)
		if sc.Recall10 > 0 {
			s.Hit10++
		}
	}
	n := float64(len(results))
	return Summary{Recall5: round(s.Recall5 / n), Recall10: round(s.Recall10 / n), Hit10: round(s.Hit10 / n), Tokens: math.Round(s.Tokens / n), Errors: s.Errors}
}

func round(f float64) float64 { return math.Round(f*1000) / 1000 }

// Regressions compares a run with a baseline: any recall or hit rate that
// fell by more than tolerance, or tokens that grew by more than tolerance
// (as a share), is a regression.
func Regressions(got, base Report, tolerance float64) []string {
	var out []string
	for _, tool := range []string{ToolMinimalContext, ToolSearch} {
		g, b := got.Summary[tool], base.Summary[tool]
		for _, m := range []struct {
			name      string
			got, base float64
		}{{"recall@5", g.Recall5, b.Recall5}, {"recall@10", g.Recall10, b.Recall10}, {"hit@10", g.Hit10, b.Hit10}} {
			if m.got < m.base-tolerance {
				out = append(out, fmt.Sprintf("%s %s fell from %.3f to %.3f", tool, m.name, m.base, m.got))
			}
		}
		if b.Tokens > 0 && g.Tokens > b.Tokens*(1+tolerance) {
			out = append(out, fmt.Sprintf("%s tokens grew from %.0f to %.0f", tool, b.Tokens, g.Tokens))
		}
		if g.Errors > b.Errors {
			out = append(out, fmt.Sprintf("%s errors grew from %d to %d", tool, b.Errors, g.Errors))
		}
	}
	return out
}
