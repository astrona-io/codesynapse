// Package aitasks generates reviews and reports with a language model,
// using the graph to keep prompts small: each task packs only the context it
// needs (findings, symbol cards, summaries) into one request, and the answer
// is parsed into a review that people can act on.
package aitasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/agentctx"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/query"
	"github.com/astrona-io/codesynapse/internal/review"
	"github.com/astrona-io/codesynapse/internal/reviews"
	"github.com/astrona-io/codesynapse/internal/security"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/rules"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

// Store is what tasks read from and write to.
type Store interface {
	store.Reader
	store.ReviewStore
}

// Runner executes tasks.
type Runner struct {
	St        Store
	LLM       *llm.Registry
	Authorize func(context.Context) error
}

// Input selects what a task works on.
type Input struct {
	Repo string
	// OrgID stamps the review with the organisation that asked for it.
	OrgID    string
	Provider string // optional override of the task's routed provider
	Diff     []byte // review task: analyse this change instead of hotspots
	// OnlyFindings limits triage to these finding IDs (new findings after a sync).
	OnlyFindings []string
	// Title overrides the generated review title.
	Title string
}

// ErrNothingToDo means the task had no input (no findings, no changes);
// background runs treat it as success without a result.
var ErrNothingToDo = errors.New("nothing to analyse")

// Run executes task and stores its result, returning the review id.
func (r Runner) Run(ctx context.Context, task string, in Input) (string, error) {
	if _, err := execution.Require(ctx); err != nil {
		return "", err
	}
	if r.Authorize != nil {
		if err := r.Authorize(ctx); err != nil {
			return "", err
		}
	}
	if r.LLM == nil {
		return "", errors.New("no AI providers configured")
	}
	provider, err := r.LLM.ProviderFor(task, in.Provider)
	if err != nil {
		return "", err
	}
	ctx = llm.WithTask(telemetry.WithTenant(ctx, in.OrgID), task)
	cfg, _ := r.LLM.Config(provider)
	repoNode, err := r.St.Node(ctx, model.NodeID(in.Repo, model.KindRepository, in.Repo))
	if err != nil {
		return "", err
	}
	src := sourceReader{root: repoNode.Root, enabled: cfg.AllowsSource()}
	lim := limitsFor(cfg)

	var rv reviews.Review
	switch task {
	case "security-triage":
		rv, err = r.securityTriage(ctx, provider, in, src, lim)
	case "review":
		rv, err = r.codeReview(ctx, provider, in, src, lim)
	case "briefing":
		rv, err = r.briefing(ctx, provider, in, lim)
	default:
		return "", fmt.Errorf("unknown task %q", task)
	}
	if err != nil {
		return "", err
	}
	if in.Title != "" {
		rv.Title = in.Title
	}
	rv.Repo, rv.OrgID, rv.Source, rv.Commit = in.Repo, in.OrgID, "ai", repoNode.Head
	rv, err = reviews.Prepare(rv, time.Now())
	if err != nil {
		return "", fmt.Errorf("model output did not form a valid review: %w", err)
	}
	if r.Authorize != nil {
		if err := r.Authorize(ctx); err != nil {
			return "", err
		}
	}
	if err := r.St.SaveReview(ctx, rv); err != nil {
		return "", err
	}
	return rv.ID, nil
}

// limits sizes a task to the provider's budgets, so a model with a large
// context window gets more findings, more symbols and longer snippets per
// request instead of the minimum.
type limits struct {
	chars       int // preliminary byte cap; Preflight checks the complete request
	findings    int // findings triaged per run
	batch       int // findings per triage request
	symbols     int // changed symbols reviewed
	hotspots    int // hotspots reviewed when there is no diff
	symbolLines int // source lines shown per symbol
	padLines    int // lines shown on each side of a finding
	briefItems  int // findings and documents listed in a briefing
}

func limitsFor(cfg llm.ProviderConfig) limits {
	in, out := cfg.MaxInputTokens, cfg.MaxOutputTokens
	return limits{
		chars:       min(max(in, 0), llm.MaxPromptBytes),
		findings:    clamp(in/1500, 18, reviews.MaxComments),
		batch:       clamp(out/600, 6, 40),
		symbols:     clamp(in/2500, 10, 100),
		hotspots:    clamp(in/5000, 8, 50),
		symbolLines: clamp(in/400, 80, 400),
		padLines:    clamp(in/8000, 6, 20),
		briefItems:  clamp(in/2000, 5, 50),
	}
}

func clamp(v, lo, hi int) int { return max(lo, min(v, hi)) }

func (r Runner) ask(ctx context.Context, provider, system, user string) (llm.Response, error) {
	return r.LLM.Complete(ctx, provider, llm.Request{System: system, User: user})
}

// appendPrompt admits complete evidence units; a finding or symbol is never
// cut in the middle to make its request fit.
func (r Runner) appendPrompt(ctx context.Context, provider, system string, b *strings.Builder, part string, lim limits) error {
	if len(part) > lim.chars-b.Len() {
		return fmt.Errorf("%w: task prompt exceeds the assembly byte limit", llm.ErrInputBudget)
	}
	if _, err := r.LLM.Preflight(ctx, provider, llm.Request{System: system, User: b.String() + part}); err != nil {
		return err
	}
	b.WriteString(part)
	return nil
}

func author(provider string, resp llm.Response) string {
	if resp.Model == "" {
		return provider
	}
	return provider + " · " + resp.Model
}

// --- security triage ------------------------------------------------------

const triageSystem = `You are a senior application security engineer triaging static-analysis findings.
For each finding decide: "likely-exploitable", "needs-review" or "false-positive", explain why in one to three sentences using the code and context given, and give a concrete fix (code when possible).
Answer with one JSON object only, no prose before or after:
{"summary": "<two to four sentences>", "items": [{"index": <number>, "verdict": "<verdict>", "explanation": "<text>", "suggestion": "<fix>"}]}`

type triageItem struct {
	Index       int    `json:"index"`
	Verdict     string `json:"verdict"`
	Explanation string `json:"explanation"`
	Suggestion  string `json:"suggestion"`
}

func (r Runner) securityTriage(ctx context.Context, provider string, in Input, src sourceReader, lim limits) (reviews.Review, error) {
	rep, err := security.Build(ctx, r.St, in.Repo, security.Options{MinSeverity: "medium"})
	if err != nil {
		return reviews.Review{}, err
	}
	findings := rep.Findings
	if len(in.OnlyFindings) > 0 {
		findings = slices.DeleteFunc(slices.Clone(findings), func(f security.Finding) bool { return !slices.Contains(in.OnlyFindings, f.ID) })
	}
	if len(findings) == 0 {
		return reviews.Review{}, ErrNothingToDo
	}
	findings = findings[:min(len(findings), lim.findings)]
	rv := reviews.Review{Title: "AI security triage", Verdict: reviews.Comment}
	if len(in.OnlyFindings) > 0 {
		rv.Title = fmt.Sprintf("AI triage of %d new security finding(s)", len(findings))
	}
	var summaries []string
	var lastResp llm.Response
	for start := 0; start < len(findings); {
		batch := findings[start:min(start+lim.batch, len(findings))]
		prompt, used, err := r.triagePrompt(ctx, provider, in.Repo, batch, src, lim)
		if err != nil {
			return reviews.Review{}, err
		}
		start += len(used)
		resp, err := r.ask(ctx, provider, triageSystem, prompt)
		if err != nil {
			if len(rv.Comments) > 0 {
				break // keep what earlier batches produced
			}
			return reviews.Review{}, err
		}
		lastResp = resp
		var out struct {
			Summary string       `json:"summary"`
			Items   []triageItem `json:"items"`
		}
		if err := decodeJSON(resp.Text, &out); err != nil {
			if len(rv.Comments) > 0 {
				continue
			}
			return reviews.Review{}, err
		}
		if out.Summary != "" {
			summaries = append(summaries, out.Summary)
		}
		for _, it := range out.Items {
			if it.Index < 0 || it.Index >= len(used) {
				continue // the model referenced a finding it was not given
			}
			rv.Comments = append(rv.Comments, triageComment(used[it.Index], it, &rv))
		}
	}
	rv.Summary = strings.Join(summaries, "\n\n")
	rv.Author = author(provider, lastResp)
	return rv, nil
}

func (r Runner) triagePrompt(ctx context.Context, provider, repo string, findings []security.Finding, src sourceReader, lim limits) (string, []security.Finding, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Repository %s. Findings, most important first:\n", repo)
	var used []security.Finding
	for i, f := range findings {
		var part strings.Builder
		fmt.Fprintf(&part, "\n### Finding %d\nrule: %s (%s, %s)\nlocation: %s\nexposed from an entry point: %t; reached by a test: %t\ndetail: %s\n",
			i, f.Rule, f.Severity, strings.Join(f.CWE, " "), f.Location, f.Exposed, f.Tested, f.Detail)
		if g, ok := rules.Get(f.Rule); ok {
			fmt.Fprintf(&part, "why it matters: %s\nusual fix: %s\n", g.Why, g.Fix)
		}
		if f.Owner != nil {
			fmt.Fprintf(&part, "in: %s %s\n", f.Owner.Name, f.Owner.Location)
		}
		if snip := src.around(f.Location, lim.padLines); snip != "" {
			fmt.Fprintf(&part, "code:\n```\n%s\n```\n", snip)
		}
		if err := r.appendPrompt(ctx, provider, triageSystem, &b, part.String(), lim); err != nil {
			if errors.Is(err, llm.ErrInputBudget) && len(used) > 0 {
				break
			}
			return "", nil, err
		}
		used = append(used, f)
	}
	return b.String(), used, nil
}

func triageComment(f security.Finding, it triageItem, rv *reviews.Review) reviews.CommentItem {
	file, line := splitLocation(f.Location)
	sev := f.Severity
	label := map[string]string{"likely-exploitable": "Likely exploitable", "needs-review": "Needs review", "false-positive": "Probably a false positive"}[it.Verdict]
	if label == "" {
		label = "Needs review"
	}
	if it.Verdict == "false-positive" {
		sev = "info"
	}
	if it.Verdict == "likely-exploitable" {
		rv.Verdict = reviews.RequestChanges
	}
	return reviews.CommentItem{File: file, Line: line, Severity: sev, Category: "security", Rule: f.Rule,
		Message: label + ": " + it.Explanation, Suggestion: it.Suggestion}
}

// --- code review ----------------------------------------------------------

const reviewSystem = `You are a meticulous senior engineer doing code review. Look for bugs, security problems, missing tests, error handling, concurrency and performance issues, and maintainability problems. Only comment where you can point to a specific line in the code shown. Prefer a few important comments over many small ones.
Answer with one JSON object only:
{"summary": "<overall assessment>", "verdict": "approve" | "comment" | "request_changes", "comments": [{"file": "<path as given>", "line": <number>, "severity": "info|low|medium|high|critical", "category": "security|bug|performance|maintainability|style|test|docs|other", "message": "<what and why>", "suggestion": "<concrete fix; replacement code for that line goes in a fenced code block (three backticks), which the review page shows as a diff>"}]}`

func (r Runner) codeReview(ctx context.Context, provider string, in Input, src sourceReader, lim limits) (reviews.Review, error) {
	ac := agentctx.Service{R: r.St}
	var targets []model.Node
	title := "AI review of the riskiest code"
	if len(in.Diff) > 0 {
		changes, err := gitx.ParseDiff(in.Diff)
		if err != nil {
			return reviews.Review{}, err
		}
		rep, err := review.Analyze(ctx, query.Service{R: r.St}, in.Repo, changes, review.Options{})
		if err != nil {
			return reviews.Review{}, err
		}
		for _, c := range rep.Changes[:min(len(rep.Changes), lim.symbols)] {
			if n, err := r.St.Node(ctx, c.ID); err == nil {
				targets = append(targets, n)
			}
		}
		title = "AI review of a change"
	} else {
		hs, err := query.Service{R: r.St}.Hotspots(ctx, in.Repo, lim.hotspots)
		if err != nil {
			return reviews.Review{}, err
		}
		for _, h := range hs {
			if n, err := r.St.Node(ctx, h.ID); err == nil {
				targets = append(targets, n)
			}
		}
	}
	if len(targets) == 0 {
		return reviews.Review{}, ErrNothingToDo
	}
	var b strings.Builder
	files := map[string]bool{}
	fmt.Fprintf(&b, "Repository %s. Review these symbols:\n", in.Repo)
	for _, n := range targets {
		card, err := ac.SymbolContext(ctx, in.Repo, n.ID, agentctx.ContextOptions{})
		if err != nil {
			continue
		}
		var part strings.Builder
		fmt.Fprintf(&part, "\n### %s (%s)\nfile: %s lines %d-%d\nsignature: %s\n", card.Symbol.QualifiedID, n.Kind, n.File, n.StartLine, n.EndLine, n.Signature)
		if n.Summary != "" {
			fmt.Fprintf(&part, "doc: %s\n", n.Summary)
		}
		if card.Risk != nil {
			fmt.Fprintf(&part, "risk %d (%s): %s\n", card.Risk.Score, card.Risk.Level, strings.Join(card.Risk.Reasons, "; "))
		}
		fmt.Fprintf(&part, "callers: %d, tests reaching it: %d\n", card.Counts.Callers, card.Counts.Tests)
		for _, f := range card.Findings {
			fmt.Fprintf(&part, "finding: %s\n", f.QualifiedID)
		}
		if snip := src.lines(n.File, n.StartLine, min(n.EndLine, n.StartLine+lim.symbolLines)); snip != "" {
			fmt.Fprintf(&part, "code (line numbers prefixed):\n```\n%s\n```\n", snip)
		}
		if err := r.appendPrompt(ctx, provider, reviewSystem, &b, part.String(), lim); err != nil {
			if errors.Is(err, llm.ErrInputBudget) && len(files) > 0 {
				break
			}
			return reviews.Review{}, err
		}
		files[n.File] = true
	}
	if len(files) == 0 {
		return reviews.Review{}, ErrNothingToDo
	}
	resp, err := r.ask(ctx, provider, reviewSystem, b.String())
	if err != nil {
		return reviews.Review{}, err
	}
	var out struct {
		Summary  string                `json:"summary"`
		Verdict  string                `json:"verdict"`
		Comments []reviews.CommentItem `json:"comments"`
	}
	if err := decodeJSON(resp.Text, &out); err != nil {
		return reviews.Review{}, err
	}
	rv := reviews.Review{Title: title, Summary: out.Summary, Verdict: out.Verdict, Author: author(provider, resp)}
	if !slices.Contains([]string{reviews.Approve, reviews.Comment, reviews.RequestChanges}, rv.Verdict) {
		rv.Verdict = reviews.Comment
	}
	for _, c := range out.Comments {
		if !files[strings.TrimPrefix(c.File, "./")] || strings.TrimSpace(c.Message) == "" {
			continue // only keep comments on code the model was shown
		}
		c.Severity = oneOf(c.Severity, []string{"info", "low", "medium", "high", "critical"}, "medium")
		c.Category = oneOf(c.Category, []string{"security", "bug", "performance", "maintainability", "style", "test", "docs", "other"}, "other")
		c.EndLine = 0
		rv.Comments = append(rv.Comments, c)
	}
	return rv, nil
}

// --- briefing -------------------------------------------------------------

const briefingSystem = `You write concise technical briefings for engineers joining a codebase. Use only the facts given. Write Markdown with these sections: "What this is", "Architecture", "Quality and risk", "Recommended next steps" (a numbered list of at most five concrete actions). At most 450 words.`

func (r Runner) briefing(ctx context.Context, provider string, in Input, lim limits) (reviews.Review, error) {
	ov, err := query.Service{R: r.St}.Overview(ctx, in.Repo)
	if err != nil {
		return reviews.Review{}, err
	}
	sec, err := security.Build(ctx, r.St, in.Repo, security.Options{MinSeverity: "high"})
	if err != nil {
		return reviews.Review{}, err
	}
	var b strings.Builder
	st := ov.Stats
	fmt.Fprintf(&b, "Repository: %s\nFiles by format: %v\nLines of code: %d; functions: %d; tests: %d; average complexity %.1f\n", in.Repo, st.Formats, st.TotalLOC, st.Nodes[model.KindFunction]+st.Nodes[model.KindMethod], st.Tests, st.AvgComplexity)
	fmt.Fprintf(&b, "Documented public API: %d of %d. Kubernetes resources: %d. Helm charts: %d.\n", st.Documented, st.Exported, st.K8sResources, st.HelmCharts)
	fmt.Fprintf(&b, "Security grade %s (%d/100); findings %v; vulnerabilities %v.\n", sec.Grade, sec.Score, sec.FindingCounts, sec.VulnCounts)
	if _, err := r.LLM.Preflight(ctx, provider, llm.Request{System: briefingSystem, User: b.String()}); err != nil {
		return reviews.Review{}, err
	}
	for _, f := range sec.Findings[:min(len(sec.Findings), lim.briefItems)] {
		part := fmt.Sprintf("- finding: %s at %s\n", f.Title, f.Location)
		if err := r.appendPrompt(ctx, provider, briefingSystem, &b, part, lim); err != nil {
			if errors.Is(err, llm.ErrInputBudget) {
				break
			}
			return reviews.Review{}, err
		}
	}
	for i, p := range ov.Packages {
		part := fmt.Sprintf("- %s: %s\n", p.Name, p.Summary)
		if i == 0 {
			part = "\nPackages:\n" + part
		}
		if err := r.appendPrompt(ctx, provider, briefingSystem, &b, part, lim); err != nil {
			if errors.Is(err, llm.ErrInputBudget) {
				break
			}
			return reviews.Review{}, err
		}
	}
	for i, d := range ov.Docs[:min(len(ov.Docs), lim.briefItems)] {
		part := fmt.Sprintf("- %s: %s\n", d.Name, d.Summary)
		if i == 0 {
			part = "\nDocuments:\n" + part
		}
		if err := r.appendPrompt(ctx, provider, briefingSystem, &b, part, lim); err != nil {
			if errors.Is(err, llm.ErrInputBudget) {
				break
			}
			return reviews.Review{}, err
		}
	}
	resp, err := r.ask(ctx, provider, briefingSystem, b.String())
	if err != nil {
		return reviews.Review{}, err
	}
	return reviews.Review{Kind: reviews.KindReport, Title: "AI briefing: " + in.Repo, Summary: resp.Text, Verdict: reviews.Comment, Author: author(provider, resp)}, nil
}

// --- helpers --------------------------------------------------------------

func oneOf(v string, allowed []string, def string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if slices.Contains(allowed, v) {
		return v
	}
	return def
}

// decodeJSON parses the first JSON object in a model answer, tolerating
// code fences and text around it.
func decodeJSON(text string, out any) error {
	start := strings.IndexByte(text, '{')
	end := strings.LastIndexByte(text, '}')
	if start < 0 || end <= start {
		return fmt.Errorf("the model did not return JSON (got %d characters of text)", len(text))
	}
	if err := json.Unmarshal([]byte(text[start:end+1]), out); err != nil {
		return fmt.Errorf("the model returned malformed JSON: %w", err)
	}
	return nil
}

func splitLocation(loc string) (string, int) {
	file, rest, ok := strings.Cut(loc, ":")
	if !ok {
		return loc, 0
	}
	start, _, _ := strings.Cut(rest, "-")
	n := 0
	_, _ = fmt.Sscanf(start, "%d", &n) //nolint:errcheck // 0 when absent
	return file, n
}

// sourceReader reads short snippets from the repository checkout, confined
// to it with os.Root so paths cannot escape (no .., no symlinks out).
type sourceReader struct {
	root    string
	enabled bool
}

const maxSnippetFile = 1 << 20

func (s sourceReader) lines(file string, from, to int) string {
	if !s.enabled || s.root == "" || file == "" || from <= 0 {
		return ""
	}
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return ""
	}
	defer root.Close() //nolint:errcheck // read-only
	f, err := root.Open(file)
	if err != nil {
		return ""
	}
	defer f.Close() //nolint:errcheck // read-only
	data, err := io.ReadAll(io.LimitReader(f, maxSnippetFile))
	if err != nil {
		return ""
	}
	all := strings.Split(string(data), "\n")
	var b strings.Builder
	for i := max(from, 1); i <= to && i <= len(all); i++ {
		fmt.Fprintf(&b, "%d: %s\n", i, all[i-1])
	}
	return strings.TrimRight(b.String(), "\n")
}

func (s sourceReader) around(location string, pad int) string {
	file, line := splitLocation(location)
	if line == 0 {
		return ""
	}
	return s.lines(file, max(1, line-pad), line+pad)
}
