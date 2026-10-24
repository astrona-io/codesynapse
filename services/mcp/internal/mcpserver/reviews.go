package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/reviews"
	"github.com/astrona-io/codesynapse/internal/store"
)

// registerReviewTools adds the review workflow. submit_review and
// update_review_comment are the only tools that write, and they write only
// to the review store, never to code or the graph.
func registerReviewTools(s *mcp.Server, h *handlers, rs store.ReviewStore) {
	h.reviews = rs
	read := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}
	notDestructive := false
	write := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &notDestructive}
	mcp.AddTool(s, &mcp.Tool{Name: "submit_review", Annotations: write, Description: "Submit a code review to CodeSynapse so people see it in the UI: title, summary, verdict (approve, comment, request_changes) and comments anchored to file:line with severity, category, message and an optional suggested fix. Writes only to the review store; code is never changed. Returns the review id and its page."}, h.submitReview)
	mcp.AddTool(s, &mcp.Tool{Name: "update_review_comment", Annotations: write, Description: "Record a decision on a review comment: accepted (agreed, to fix), rejected (disagree or false positive), deferred, resolved (fixed) or open again, with a short note such as the commit that fixed it."}, h.updateReviewComment)
	mcp.AddTool(s, &mcp.Tool{Name: "list_reviews", Annotations: read, Description: "List reviews of a repository, newest first, with counts of open, accepted and resolved comments."}, h.listReviews)
	mcp.AddTool(s, &mcp.Tool{Name: "get_review", Annotations: read, Description: "Get one review with its comments. status=accepted returns the comments a person agreed should be fixed (your to-do list); status=open returns those not decided yet."}, h.getReview)
}

type reviewCommentIn struct {
	File       string `json:"file" jsonschema:"repo-relative path"`
	Line       int    `json:"line,omitempty" jsonschema:"first line (1-based)"`
	EndLine    int    `json:"end_line,omitempty" jsonschema:"last line, for ranges"`
	Severity   string `json:"severity,omitempty" jsonschema:"info, low, medium (default), high or critical"`
	Category   string `json:"category,omitempty" jsonschema:"security, bug, performance, maintainability, style, test, docs or other"`
	Message    string `json:"message" jsonschema:"what is wrong and why, for a human reader"`
	Suggestion string `json:"suggestion,omitempty" jsonschema:"concrete fix in Markdown; put replacement code in a fenced code block that replaces exactly lines line..end_line, so the review page can show it as a diff"`
	Rule       string `json:"rule,omitempty" jsonschema:"security rule id if the comment is about a finding (see rule_guide)"`
}

type submitReviewIn struct {
	RepoArg
	Title    string            `json:"title" jsonschema:"short title, e.g. 'Review of auth refactor'"`
	Summary  string            `json:"summary,omitempty" jsonschema:"overall assessment in a few sentences (Markdown allowed)"`
	Verdict  string            `json:"verdict,omitempty" jsonschema:"approve, comment (default) or request_changes"`
	Kind     string            `json:"kind,omitempty" jsonschema:"review (default) or report: a briefing written in summary, with no comments"`
	Base     string            `json:"base,omitempty" jsonschema:"base ref or commit reviewed against"`
	Head     string            `json:"head,omitempty" jsonschema:"head ref or commit reviewed"`
	Comments []reviewCommentIn `json:"comments,omitempty" jsonschema:"at most 200"`
}

type submitReviewOut struct {
	ID       string         `json:"id"`
	Page     string         `json:"page"`
	Counts   reviews.Counts `json:"counts"`
	Warnings []string       `json:"warnings,omitempty"`
}

func clientName(req *mcp.CallToolRequest) string {
	if req == nil {
		return ""
	}
	return sessionClient(req)
}

func (h *handlers) submitReview(ctx context.Context, req *mcp.CallToolRequest, in submitReviewIn) (*mcp.CallToolResult, submitReviewOut, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, submitReviewOut{}, err
	}
	if err := h.canWrite(ctx, repo); err != nil {
		return nil, submitReviewOut{}, err
	}
	repoNode, err := h.svc.R.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		return nil, submitReviewOut{}, err
	}
	r := reviews.Review{
		Repo: repo, OrgID: reviewOrg(ctx), Kind: in.Kind, Title: in.Title, Summary: in.Summary, Verdict: in.Verdict, Base: in.Base, Head: in.Head,
		Author: clientName(req), Source: "mcp", Commit: repoNode.Head,
	}
	for _, c := range in.Comments {
		r.Comments = append(r.Comments, reviews.CommentItem{File: c.File, Line: c.Line, EndLine: c.EndLine, Severity: c.Severity,
			Category: c.Category, Message: c.Message, Suggestion: c.Suggestion, Rule: c.Rule})
	}
	r, err = reviews.Prepare(r, time.Now())
	if err != nil {
		return nil, submitReviewOut{}, err
	}
	// Anchor comments to the symbol at their line, and warn about files the
	// graph does not know (typos, or files changed since the last ingest).
	var warnings []string
	for i := range r.Comments {
		c := &r.Comments[i]
		ns, err := h.svc.R.NodesInFile(ctx, repo, c.File)
		if err != nil {
			return nil, submitReviewOut{}, err
		}
		if len(ns) == 0 {
			warnings = append(warnings, fmt.Sprintf("%s is not in the graph (new file, or not indexed yet)", c.File))
			continue
		}
		if c.Line > 0 {
			end := max(c.EndLine, c.Line)
			if syms, err := h.svc.SymbolsAt(ctx, repo, c.File, c.Line, end); err == nil && len(syms) > 0 {
				c.Symbol, c.SymbolID = syms[0].ShortName(), syms[0].ID
			}
		}
	}
	if err := h.reviews.SaveReview(ctx, r); err != nil {
		return nil, submitReviewOut{}, err
	}
	return nil, submitReviewOut{ID: r.ID, Page: "/repos/" + repo + "/reviews?id=" + r.ID, Counts: r.Counts(), Warnings: dedupeStrings(warnings)}, nil
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

type updateCommentIn struct {
	ReviewID  string `json:"review_id"`
	CommentID string `json:"comment_id"`
	Status    string `json:"status" jsonschema:"open, accepted, rejected, deferred or resolved (wontfix is accepted as rejected)"`
	Note      string `json:"note,omitempty" jsonschema:"why, or the commit that fixed it"`
}

func (h *handlers) updateReviewComment(ctx context.Context, req *mcp.CallToolRequest, in updateCommentIn) (*mcp.CallToolResult, reviews.Counts, error) {
	r, err := h.reviews.GetReview(ctx, in.ReviewID)
	if err != nil {
		return nil, reviews.Counts{}, err
	}
	if err := h.canSeeReview(ctx, r.Repo, r.OrgID); err != nil {
		return nil, reviews.Counts{}, err
	}
	if err := h.canWrite(ctx, r.Repo); err != nil {
		return nil, reviews.Counts{}, err
	}
	by := clientName(req)
	if by == "" {
		by = "mcp"
	}
	if err := reviews.ApplyStatus(&r, in.CommentID, in.Status, in.Note, by, time.Now()); err != nil {
		return nil, reviews.Counts{}, err
	}
	if err := h.reviews.SaveReview(ctx, r); err != nil {
		return nil, reviews.Counts{}, err
	}
	return nil, r.Counts(), nil
}

type listReviewsOut struct {
	Reviews []reviews.Summary `json:"reviews"`
}

func (h *handlers) listReviews(ctx context.Context, _ *mcp.CallToolRequest, in limitIn) (*mcp.CallToolResult, listReviewsOut, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, listReviewsOut{}, err
	}
	list, err := h.reviews.ListReviews(ctx, repo, in.Limit)
	if err != nil {
		return nil, listReviewsOut{}, err
	}
	if c := callerFrom(ctx); c != nil {
		list = slices.DeleteFunc(list, func(r reviews.Review) bool { return r.OrgID != c.OrgID })
	}
	out := listReviewsOut{Reviews: []reviews.Summary{}}
	for _, r := range list {
		out.Reviews = append(out.Reviews, reviews.SummaryOf(r))
	}
	return nil, out, nil
}

type getReviewIn struct {
	ID     string `json:"id"`
	Status string `json:"status,omitempty" jsonschema:"accepted (agreed, to fix), open (undecided), deferred, rejected or resolved"`
}

func (h *handlers) getReview(ctx context.Context, _ *mcp.CallToolRequest, in getReviewIn) (*mcp.CallToolResult, reviews.Review, error) {
	if strings.TrimSpace(in.ID) == "" {
		return nil, reviews.Review{}, errors.New("id is required")
	}
	r, err := h.reviews.GetReview(ctx, in.ID)
	if err != nil {
		return nil, r, err
	}
	if err := h.canSeeReview(ctx, r.Repo, r.OrgID); err != nil {
		return nil, reviews.Review{}, err
	}
	if in.Status != "" {
		kept := r.Comments[:0:0]
		for _, c := range r.Comments {
			if c.Status == in.Status {
				kept = append(kept, c)
			}
		}
		r.Comments = kept
	}
	return nil, r, nil
}
