// Package agentctx builds the composite answers behind the agent-facing MCP
// tools (docs/AGENT_GUIDE.md): one call returns what would otherwise take an
// agent several tool calls or file reads. Every answer is bounded, and the
// "outline" verbosity is the cheap default.
package agentctx

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/duplicates"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/query"
	"github.com/astrona-io/codesynapse/internal/review"
	"github.com/astrona-io/codesynapse/internal/security"
	"github.com/astrona-io/codesynapse/internal/store"
)

// Verbosity levels.
const (
	Outline = "outline"
	Full    = "full"
)

var callEdges = []model.EdgeKind{model.EdgeCalls, model.EdgeDispatchesTo}

// Service composes graph reads into agent answers.
type Service struct {
	R store.Reader
}

func (s Service) q() query.Service { return query.Service{R: s.R} }

// Symbol is the compact description of the subject of an answer.
type Symbol struct {
	ID          string         `json:"id"`
	SymbolID    string         `json:"symbol_id,omitempty"`
	Revision    string         `json:"revision,omitempty"`
	QualifiedID string         `json:"qualified_id"`
	Kind        model.NodeKind `json:"kind"`
	Location    string         `json:"location,omitempty"`
	Signature   string         `json:"signature,omitempty"`
	Summary     string         `json:"summary,omitempty"`
	Exported    bool           `json:"exported,omitempty"`
	IsTest      bool           `json:"is_test,omitempty"`
	Complexity  int            `json:"complexity,omitempty"`
	LOC         int            `json:"loc,omitempty"`
	Tags        []string       `json:"security_tags,omitempty"`
	// Liveness is live, test-only or unreachable (see unused_code).
	Liveness string `json:"liveness,omitempty"`
}

func symbolOf(n model.Node) Symbol {
	return Symbol{
		ID: n.ID, QualifiedID: n.ShortName(), Kind: n.Kind, Location: n.Location(), Signature: n.Signature,
		SymbolID: n.SymbolID, Revision: n.Revision,
		Summary: n.Summary, Exported: n.Exported, IsTest: n.IsTest, Complexity: n.Complexity, LOC: n.LOC, Tags: n.SecurityTags, Liveness: n.Liveness,
	}
}

// Ref is a related node. EntryPoint and Exported help an agent judge
// impact without another lookup.
type Ref struct {
	QualifiedID string         `json:"qualified_id"`
	Kind        model.NodeKind `json:"kind"`
	Location    string         `json:"location,omitempty"`
	Depth       int            `json:"depth,omitempty"`
	Exported    bool           `json:"exported,omitempty"`
	IsTest      bool           `json:"is_test,omitempty"`
	EntryPoint  bool           `json:"entry_point,omitempty"`
	Tags        []string       `json:"security_tags,omitempty"`
	Summary     string         `json:"summary,omitempty"`
	// Evidence says how a direct caller's or callee's edge was established.
	Evidence model.Evidence `json:"evidence,omitempty"`
}

func refOf(n model.Node, depth int, withSummary bool) Ref {
	r := Ref{
		QualifiedID: n.ShortName(), Kind: n.Kind, Location: n.Location(), Depth: depth,
		Exported: n.Exported, IsTest: n.IsTest, EntryPoint: n.Kind.IsCallable() && security.IsEntryPoint(n), Tags: n.SecurityTags,
	}
	if withSummary {
		r.Summary = n.Summary
	}
	return r
}

// Risk is the change-risk score of a symbol (see review.Score).
type Risk struct {
	Score   int          `json:"score"`
	Level   review.Level `json:"level"`
	Reasons []string     `json:"reasons"`
}

// Churn is recent change activity of the symbol's file.
type Churn struct {
	File          string      `json:"file"`
	Commits       int         `json:"commits"`
	RecentCommits []query.Ref `json:"recent_commits,omitempty"`
}

// SymbolContext answers get_symbol_context for a code symbol.
type SymbolContext struct {
	Budget          ResponseBudget `json:"budget"`
	Symbol          Symbol         `json:"symbol"`
	Risk            *Risk          `json:"risk,omitempty"`
	Counts          Counts         `json:"counts"`
	Callers         []Ref          `json:"callers"`
	Callees         []Ref          `json:"callees"`
	Tests           []Ref          `json:"tests"`
	UntestedCallers []Ref          `json:"untested_public_callers,omitempty"`
	Implementations []Ref          `json:"implementations,omitempty"`
	Docs            []Ref          `json:"docs,omitempty"`
	Findings        []Ref          `json:"findings,omitempty"`
	Churn           *Churn         `json:"churn,omitempty"`
	Package         *PackageCtx    `json:"package,omitempty"`
	Analysis        *AnalysisNote  `json:"analysis,omitempty"`
	// Duplicate is set when the function has exact or edited copies elsewhere.
	Duplicate *duplicates.Info `json:"duplicate,omitempty"`
	Hint      string           `json:"hint,omitempty"`
}

// Counts are totals before list truncation.
type Counts struct {
	Callers            int `json:"callers"`
	Callees            int `json:"callees"`
	Tests              int `json:"tests"`
	Implementations    int `json:"implementations,omitempty"`
	CallerTestsChecked int `json:"caller_tests_checked,omitempty"`
	CallerTestsOmitted int `json:"caller_tests_omitted,omitempty"`
}

// ContextOptions tune SymbolContext.
type ContextOptions struct {
	Verbosity string
	// Depth is the caller depth (1 = direct). Outline always uses 1.
	Depth     int
	Limit     int
	MaxTokens int
}

// SymbolContext builds the context for a symbol or package.
func (s Service) SymbolContext(ctx context.Context, repo, symbol string, opts ContextOptions) (SymbolContext, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	s.R = store.ForRepository(s.R, repo)
	full := opts.Verbosity == Full
	limit := store.ClampLimit(opts.Limit, 5)
	depth := 1
	if full {
		limit = store.ClampLimit(opts.Limit, 20)
		depth = min(max(opts.Depth, 1), 4)
	}
	n, err := s.Resolve(ctx, repo, symbol)
	if err != nil {
		return SymbolContext{}, err
	}
	if n.Kind == model.KindPackage {
		pc, err := s.packageContext(ctx, repo, n, limit)
		if err != nil {
			return SymbolContext{}, err
		}
		return finishSymbol(SymbolContext{Symbol: symbolOf(n), Package: &pc, Callers: []Ref{}, Callees: []Ref{}, Tests: []Ref{}}, opts.MaxTokens)
	}

	out := SymbolContext{Symbol: symbolOf(n)}
	callers, err := s.R.Traverse(ctx, n.ID, callEdges, store.In, max(depth, 3), store.MaxLimit)
	if err != nil {
		return out, err
	}
	var direct, tests []model.Hit
	for _, h := range callers {
		switch {
		case h.Node.IsTest:
			tests = append(tests, h)
		case h.Depth <= depth:
			direct = append(direct, h)
		}
	}
	out.Counts.Callers, out.Counts.Tests = len(direct), len(tests)
	out.Callers = refs(direct, limit, full)
	out.Tests = refs(tests, limit, full)

	callees, err := s.R.Traverse(ctx, n.ID, []model.EdgeKind{model.EdgeCalls}, store.Out, 1, store.MaxLimit)
	if err != nil {
		return out, err
	}
	out.Counts.Callees = len(callees)
	out.Callees = refs(callees, limit, full)
	if err := s.labelEvidence(ctx, n.ID, direct, out.Callers, callees, out.Callees); err != nil {
		return out, err
	}
	if err := s.labelEvidence(ctx, n.ID, tests, out.Tests, nil, nil); err != nil {
		return out, err
	}
	if out.Analysis, err = s.analysisOf(ctx, repo, n); err != nil {
		return out, err
	}

	impls, err := s.Implementations(ctx, repo, n.ID, limit)
	if err == nil && len(impls.Results) > 0 {
		out.Implementations, out.Counts.Implementations = impls.Results, impls.Total
	}

	findings, err := s.R.Traverse(ctx, n.ID, []model.EdgeKind{model.EdgeHasFinding}, store.Out, 1, 20)
	if err != nil {
		return out, err
	}
	var sevs []string
	for _, f := range findings {
		sevs = append(sevs, f.Node.Severity)
		r := refOf(f.Node, 0, true)
		r.QualifiedID = f.Node.Severity + " " + f.Node.Rule
		out.Findings = append(out.Findings, r)
	}

	if n.Kind.IsCallable() {
		fanIn := 0
		for _, h := range direct {
			if h.Depth == 1 {
				fanIn++
			}
		}
		score, reasons := review.Score(review.Signals{
			FanIn: fanIn, Impacted: len(callers) - len(tests), Exported: n.Exported, Complexity: n.Complexity,
			Tags: n.SecurityTags, Tested: len(tests) > 0 || n.IsTest, Findings: sevs,
		})
		out.Risk = &Risk{Score: score, Level: review.LevelFor(score), Reasons: reasons}
	}

	out.Duplicate = query.DuplicateOf(ctx, s.R, repo, n)
	out.Duplicate = s.scopedDuplicate(ctx, out.Duplicate)

	if full {
		// Public callers with no test of their own: where a refactor can break
		// behaviour nobody checks.
		for _, h := range direct {
			if !h.Node.Exported && !security.IsEntryPoint(h.Node) {
				continue
			}
			if out.Counts.CallerTestsChecked >= limit {
				out.Counts.CallerTestsOmitted++
				continue
			}
			out.Counts.CallerTestsChecked++
			t, err := s.q().Tests(ctx, h.Node.ID, 3, 1)
			if err != nil {
				return out, err
			}
			if len(t) == 0 {
				out.UntestedCallers = append(out.UntestedCallers, refOf(h.Node, h.Depth, false))
			}
		}
		if out.Counts.CallerTestsOmitted > 0 {
			out.Hint = fmt.Sprintf("Test coverage was checked for %d of %d public or entry-point callers; increase limit or narrow the symbol before drawing conclusions.", out.Counts.CallerTestsChecked, out.Counts.CallerTestsChecked+out.Counts.CallerTestsOmitted)
		}
		docs, err := s.R.Traverse(ctx, n.ID, []model.EdgeKind{model.EdgeMentions}, store.In, 1, limit)
		if err != nil {
			return out, err
		}
		out.Docs = refs(docs, limit, true)
		if n.File != "" {
			churn, err := s.churn(ctx, repo, n.File)
			if err != nil {
				return out, err
			}
			out.Churn = churn
		}
	} else {
		out.Hint = "verbosity=full adds transitive callers, untested public callers, docs and file churn"
	}
	return finishSymbol(out, opts.MaxTokens)
}

func refs(hits []model.Hit, limit int, withSummary bool) []Ref {
	out := make([]Ref, 0, min(len(hits), limit))
	for _, h := range hits[:min(len(hits), limit)] {
		out = append(out, refOf(h.Node, h.Depth, withSummary))
	}
	return out
}

func (s Service) churn(ctx context.Context, repo, file string) (*Churn, error) {
	s.R = store.ForRepository(s.R, repo)
	f, err := s.R.Node(ctx, model.NodeID(repo, model.KindFile, file))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	hits, err := s.R.Traverse(ctx, f.ID, []model.EdgeKind{model.EdgeModifies}, store.In, 1, 50)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(hits, func(a, b model.Hit) int { return cmp.Compare(b.Node.Time, a.Node.Time) })
	c := &Churn{File: file, Commits: f.Churn}
	for _, h := range hits[:min(len(hits), 5)] {
		c.RecentCommits = append(c.RecentCommits, query.Ref{ID: h.Node.ID, Kind: h.Node.Kind, Name: h.Node.Name + " " + h.Node.Time, Summary: h.Node.Summary})
	}
	return c, nil
}

// Resolve finds one node for a qualified ID ("store.(*Store).Get"), a full
// node ID, a plain name, or a package path. Ambiguous or unknown names
// return a SuggestionError listing qualified IDs to retry with.
func (s Service) Resolve(ctx context.Context, repo, symbol string) (model.Node, error) {
	if len(symbol) > 4096 || len(repo) > 4096 {
		return model.Node{}, errors.New("repository and symbol must not exceed 4096 bytes")
	}
	if err := ctx.Err(); err != nil {
		return model.Node{}, err
	}
	s.R = store.ForRepository(s.R, repo)
	symbol = strings.TrimSpace(symbol)
	if query.IsSymbolID(symbol) {
		return query.ResolveSymbolID(ctx, s.R, repo, symbol)
	}
	if symbol == "" {
		return model.Node{}, errors.New("symbol is required")
	}
	if strings.Count(symbol, "|") >= 2 {
		return s.R.Node(ctx, symbol)
	}
	kinds := []model.NodeKind{model.KindFunction, model.KindMethod, model.KindInterfaceMethod, model.KindType, model.KindPackage}
	// Qualified node IDs are indexed by every store. Resolve them before a
	// capped substring search can hide the exact symbol among common names.
	if strings.ContainsAny(symbol, "./") {
		for _, kind := range kinds {
			n, err := s.R.Node(ctx, model.NodeID(repo, kind, symbol))
			if err == nil {
				return n, nil
			}
			if !errors.Is(err, store.ErrNotFound) {
				return model.Node{}, err
			}
		}
	}
	needle := symbol
	if i := strings.LastIndexAny(symbol, ".)"); i >= 0 && i+1 < len(symbol) {
		needle = symbol[i+1:]
	}
	if strings.Contains(symbol, "/") && !strings.Contains(symbol, ".") {
		needle = symbol // package path
	}
	cands, err := s.R.Search(ctx, repo, needle, kinds, 50)
	if err != nil {
		return model.Node{}, err
	}
	var exact []model.Node
	for _, c := range cands {
		switch {
		case c.ID == symbol, c.ShortName() == symbol, c.QualifiedName == symbol,
			c.Name == symbol, strings.HasSuffix(c.QualifiedName, "/"+symbol), strings.HasSuffix(c.QualifiedName, "."+symbol):
			exact = append(exact, c)
		}
	}
	// Prefer code symbols over a package with the same name.
	if len(exact) > 1 {
		code := slices.DeleteFunc(slices.Clone(exact), func(n model.Node) bool { return n.Kind == model.KindPackage })
		if len(code) > 0 {
			exact = code
		}
	}
	switch {
	case len(exact) == 1:
		return exact[0], nil
	case len(exact) == 0 && len(cands) == 1:
		return cands[0], nil
	}
	pool := exact
	if len(pool) == 0 {
		pool = cands
	}
	return model.Node{}, suggestions(symbol, pool, len(exact) > 1)
}

func (s Service) scopedDuplicate(ctx context.Context, info *duplicates.Info) *duplicates.Info {
	if info == nil {
		return nil
	}
	others := make([]duplicates.Member, 0, len(info.Others))
	for _, member := range info.Others {
		n, err := s.R.Node(ctx, member.ID)
		if err != nil {
			continue
		}
		others = append(others, duplicates.Member{ID: n.ID, Name: n.ShortName(), Package: n.Package,
			File: n.File, StartLine: n.StartLine, EndLine: n.EndLine, LOC: n.LOC})
	}
	if len(others) == 0 {
		return nil
	}
	return &duplicates.Info{Kind: info.Kind, Similarity: info.Similarity, Scope: "repository", Copies: len(others) + 1, Others: others}
}

// SuggestionError tells the agent which exact IDs to use next.
type SuggestionError struct {
	Query       string   `json:"query"`
	Ambiguous   bool     `json:"ambiguous"`
	Suggestions []string `json:"suggestions"`
}

func (e *SuggestionError) Error() string {
	if len(e.Suggestions) == 0 {
		return fmt.Sprintf("no symbol matches %q; try search", e.Query)
	}
	what := "no exact match for"
	if e.Ambiguous {
		what = "several symbols match"
	}
	return fmt.Sprintf("%s %q; retry with one of these qualified_id values: %s", what, e.Query, strings.Join(e.Suggestions, ", "))
}

func suggestions(q string, pool []model.Node, ambiguous bool) error {
	seen := map[string]int{}
	for _, n := range pool {
		seen[n.ShortName()]++
	}
	var out []string
	for _, n := range pool[:min(len(pool), 10)] {
		// Short IDs that collide (two packages named "store") fall back to the full ID.
		if seen[n.ShortName()] > 1 {
			out = append(out, n.ID)
		} else {
			out = append(out, n.ShortName())
		}
	}
	return &SuggestionError{Query: q, Ambiguous: ambiguous, Suggestions: out}
}

// IsSuggestion reports whether err carries suggestions.
func IsSuggestion(err error) bool {
	var s *SuggestionError
	return errors.As(err, &s)
}
