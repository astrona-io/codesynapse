// Package reviews holds code reviews submitted to CodeSynapse, usually by an
// AI agent over MCP: a verdict, a summary and comments anchored to file and
// line, each with severity, category and an optional suggested fix.
//
// Reviews are stored apart from the code graph (re-indexing replaces the
// graph, not reviews) and never change code. All text is size-limited and
// secret-redacted on the way in, because reviews are shown to people and
// fed back to agents.
package reviews

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/parisnakitakejser/codesynapse/internal/redact"
)

// Verdicts.
const (
	Approve        = "approve"
	Comment        = "comment"
	RequestChanges = "request_changes"
)

// Kinds of review document.
const (
	KindReview = "review"
	// KindReport is a generated report (a briefing) with no comments.
	KindReport = "report"
)

// Comment statuses: the decisions a developer takes on a recommendation.
const (
	Open     = "open"     // not decided yet
	Accepted = "accepted" // agreed, to be fixed
	Rejected = "rejected" // disagreed or false positive
	Deferred = "deferred" // later
	Resolved = "resolved" // fixed
	WontFix  = "wontfix"  // legacy alias of rejected
)

// Limits on submitted reviews.
const (
	MaxComments   = 200
	maxTitle      = 200
	maxSummary    = 8000
	maxMessage    = 4000
	maxSuggestion = 8000
	maxNote       = 1000
)

var (
	verdicts   = []string{Approve, Comment, RequestChanges}
	severities = []string{"info", "low", "medium", "high", "critical"}
	categories = []string{"security", "bug", "performance", "maintainability", "style", "test", "docs", "other"}
	statuses   = []string{Open, Accepted, Rejected, Deferred, Resolved, WontFix}
)

// ErrInvalid wraps validation errors.
var ErrInvalid = errors.New("invalid review")

// CommentItem is one review comment.
type CommentItem struct {
	ID         string `json:"id"`
	File       string `json:"file"`
	Line       int    `json:"line,omitempty"`
	EndLine    int    `json:"end_line,omitempty"`
	Severity   string `json:"severity"`
	Category   string `json:"category"`
	Message    string `json:"message"`
	Suggestion string `json:"suggestion,omitempty"`
	// Rule links a comment to a security rule (rules/*/<id>.yaml).
	Rule string `json:"rule,omitempty"`
	// Symbol is the qualified_id of the symbol at File:Line, filled in by
	// CodeSynapse from the graph.
	Symbol     string `json:"symbol,omitempty"`
	SymbolID   string `json:"symbol_id,omitempty"`
	Status     string `json:"status"`
	StatusNote string `json:"status_note,omitempty"`
	StatusBy   string `json:"status_by,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
	// Fix is set by CodeSynapse when it pushes a fix branch for this comment.
	Fix *Fix `json:"fix,omitempty"`
}

// Fix states.
const (
	FixQueued  = "queued" // waiting for other jobs
	FixRunning = "running"
	FixPushed  = "pushed"
	FixFailed  = "failed"
)

// Fix is a generated fix pushed to a branch, where CI runs and a person
// reviews it before merging.
type Fix struct {
	State      string `json:"state"`
	Job        string `json:"job,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Commit     string `json:"commit,omitempty"`
	PRURL      string `json:"pr_url,omitempty"`
	CompareURL string `json:"compare_url,omitempty"`
	Model      string `json:"model,omitempty"`
	Error      string `json:"error,omitempty"`
	At         string `json:"at"`
}

// Review is a submitted review.
type Review struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Repo string `json:"repo"`
	// OrgID is the organisation the review belongs to ("" before accounts or
	// for the service token); organisations see only their own.
	OrgID     string        `json:"org_id,omitempty"`
	Title     string        `json:"title"`
	Summary   string        `json:"summary"`
	Verdict   string        `json:"verdict"`
	Author    string        `json:"author"`
	Source    string        `json:"source"`
	Base      string        `json:"base,omitempty"`
	Head      string        `json:"head,omitempty"`
	Commit    string        `json:"commit,omitempty"`
	CreatedAt string        `json:"created_at"`
	UpdatedAt string        `json:"updated_at"`
	Comments  []CommentItem `json:"comments"`
}

// Counts summarises a review's comments.
type Counts struct {
	Total int `json:"total"`
	// Open counts comments without a decision.
	Open     int `json:"open"`
	Accepted int `json:"accepted"`
	Rejected int `json:"rejected"`
	Deferred int `json:"deferred"`
	Resolved int `json:"resolved"`
	// WontFix is kept for older clients; it equals Rejected.
	WontFix    int            `json:"wontfix"`
	BySeverity map[string]int `json:"by_severity"`
}

// Counts returns comment totals.
func (r Review) Counts() Counts {
	c := Counts{Total: len(r.Comments), BySeverity: map[string]int{}}
	for _, cm := range r.Comments {
		switch cm.Status {
		case Resolved:
			c.Resolved++
		case WontFix, Rejected:
			c.Rejected++
			c.WontFix++
		case Accepted:
			c.Accepted++
		case Deferred:
			c.Deferred++
		default:
			c.Open++
		}
		if cm.Status == Open || cm.Status == Accepted {
			c.BySeverity[cm.Severity]++
		}
	}
	return c
}

// Summary is a review without its comments, for lists.
type Summary struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Repo      string `json:"repo"`
	OrgID     string `json:"org_id,omitempty"`
	Title     string `json:"title"`
	Verdict   string `json:"verdict"`
	Author    string `json:"author"`
	Source    string `json:"source"`
	Commit    string `json:"commit,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	Counts    Counts `json:"counts"`
}

// SummaryOf builds the list view of r.
func SummaryOf(r Review) Summary {
	return Summary{ID: r.ID, Kind: r.Kind, Repo: r.Repo, OrgID: r.OrgID, Title: r.Title, Verdict: r.Verdict, Author: r.Author, Source: r.Source,
		Commit: r.Commit, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Counts: r.Counts()}
}

func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b)
}

// clean redacts secrets and personal data and bounds length, keeping line
// breaks.
func clean(s string, limit int) string {
	s = strings.ToValidUTF8(strings.TrimSpace(redact.Free(s)), "")
	if utf8.RuneCountInString(s) > limit {
		r := []rune(s)
		s = string(r[:limit-1]) + "…"
	}
	return s
}

// Prepare validates and normalises a new review: IDs, timestamps,
// defaults, limits and redaction. now is injected for tests.
func Prepare(r Review, now time.Time) (Review, error) {
	r.Title = clean(r.Title, maxTitle)
	if r.Title == "" {
		return r, fmt.Errorf("%w: title is required", ErrInvalid)
	}
	if r.Verdict == "" {
		r.Verdict = Comment
	}
	if !slices.Contains(verdicts, r.Verdict) {
		return r, fmt.Errorf("%w: verdict must be one of %v", ErrInvalid, verdicts)
	}
	if len(r.Comments) > MaxComments {
		return r, fmt.Errorf("%w: at most %d comments per review", ErrInvalid, MaxComments)
	}
	if r.Kind == "" {
		r.Kind = KindReview
	}
	if r.Kind != KindReview && r.Kind != KindReport {
		return r, fmt.Errorf("%w: kind must be %s or %s", ErrInvalid, KindReview, KindReport)
	}
	r.Summary = clean(r.Summary, maxSummary)
	r.Author = clean(r.Author, 100)
	if r.Author == "" {
		r.Author = "unknown"
	}
	r.Base, r.Head = clean(r.Base, 200), clean(r.Head, 200)
	r.ID = newID()
	ts := now.UTC().Format(time.RFC3339)
	r.CreatedAt, r.UpdatedAt = ts, ts
	for i := range r.Comments {
		c := &r.Comments[i]
		c.File = strings.TrimPrefix(strings.TrimSpace(c.File), "./")
		if c.File == "" || strings.Contains(c.File, "..") || strings.HasPrefix(c.File, "/") {
			return r, fmt.Errorf("%w: comment %d needs a repo-relative file", ErrInvalid, i+1)
		}
		if c.Line < 0 || c.EndLine < 0 || (c.EndLine > 0 && c.EndLine < c.Line) {
			return r, fmt.Errorf("%w: comment %d has an invalid line range", ErrInvalid, i+1)
		}
		if c.Severity == "" {
			c.Severity = "medium"
		}
		if !slices.Contains(severities, c.Severity) {
			return r, fmt.Errorf("%w: comment %d severity must be one of %v", ErrInvalid, i+1, severities)
		}
		if c.Category == "" {
			c.Category = "other"
		}
		if !slices.Contains(categories, c.Category) {
			return r, fmt.Errorf("%w: comment %d category must be one of %v", ErrInvalid, i+1, categories)
		}
		c.Message = clean(c.Message, maxMessage)
		if c.Message == "" {
			return r, fmt.Errorf("%w: comment %d needs a message", ErrInvalid, i+1)
		}
		c.Suggestion = clean(c.Suggestion, maxSuggestion)
		c.Rule = clean(c.Rule, 80)
		c.ID = newID()
		c.Status = Open
		c.StatusNote, c.StatusBy, c.UpdatedAt = "", "", ts
		c.Fix = nil // only CodeSynapse records fixes
	}
	return r, nil
}

// SetFix records fix state on the given comments.
func SetFix(r *Review, ids []string, f Fix, now time.Time) {
	f.At = now.UTC().Format(time.RFC3339)
	f.Error = clean(f.Error, maxNote)
	for i := range r.Comments {
		if slices.Contains(ids, r.Comments[i].ID) {
			fx := f
			r.Comments[i].Fix = &fx
		}
	}
	r.UpdatedAt = f.At
}

// ApplyStatus changes one comment's status.
func ApplyStatus(r *Review, commentID, status, note, by string, now time.Time) error {
	if !slices.Contains(statuses, status) {
		return fmt.Errorf("%w: status must be one of %v", ErrInvalid, statuses)
	}
	if status == WontFix {
		status = Rejected
	}
	for i := range r.Comments {
		if r.Comments[i].ID == commentID {
			c := &r.Comments[i]
			c.Status, c.StatusNote, c.StatusBy = status, clean(note, maxNote), clean(by, 100)
			c.UpdatedAt = now.UTC().Format(time.RFC3339)
			r.UpdatedAt = c.UpdatedAt
			return nil
		}
	}
	return fmt.Errorf("%w: comment %q not found", ErrInvalid, commentID)
}
