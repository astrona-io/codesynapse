// Package query composes store reads into compact, purpose-built answers.
//
// These answers are the product: an agent asking for a SymbolCard gets the
// signature, location, summary, callers, callees, tests and docs of one
// symbol in a few hundred tokens, instead of opening and reading several
// files. The MCP server and the HTTP API both serve these types unchanged.
package query

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/parisnakitakejser/codesynapse/internal/duplicates"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/store"
)

// callEdges are followed when computing callers: static calls plus interface
// dispatch, so a change to a concrete method reaches interface callers.
var callEdges = []model.EdgeKind{model.EdgeCalls, model.EdgeDispatchesTo}

// Service answers queries against a store.
type Service struct {
	R store.Reader
}

// Ref is the compact form of a node used inside other answers.
type Ref struct {
	ID       string         `json:"id"`
	Kind     model.NodeKind `json:"kind"`
	Name     string         `json:"name"`
	Location string         `json:"location,omitempty"`
	Summary  string         `json:"summary,omitempty"`
	Depth    int            `json:"depth,omitempty"`
}

// RefOf builds a Ref from a node.
func RefOf(n model.Node) Ref {
	return Ref{ID: n.ID, Kind: n.Kind, Name: n.ShortName(), Location: n.Location(), Summary: n.Summary}
}

func refsOf(hits []model.Hit) []Ref {
	out := make([]Ref, 0, len(hits))
	for _, h := range hits {
		r := RefOf(h.Node)
		r.Depth = h.Depth
		out = append(out, r)
	}
	return out
}

// SymbolCard is everything an assistant usually needs about one symbol.
type SymbolCard struct {
	Symbol          model.Node `json:"symbol"`
	Callers         []Ref      `json:"callers"`
	Callees         []Ref      `json:"callees"`
	Tests           []Ref      `json:"tests"`
	Implementations []Ref      `json:"implementations,omitempty"`
	Docs            []Ref      `json:"docs,omitempty"`
	// Duplicate is set when the function has exact or edited copies.
	Duplicate *duplicates.Info `json:"duplicate,omitempty"`
}

// DuplicateOf looks the function up in the repository's duplicate report. A
// missing or unreadable report just means nothing to show.
func DuplicateOf(ctx context.Context, r store.Reader, repo string, n model.Node) *duplicates.Info {
	if !n.Kind.IsCallable() {
		return nil
	}
	rn, err := r.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
	if err != nil || rn.Duplicates == "" {
		return nil
	}
	rep, err := duplicates.Decode(rn.Duplicates)
	if err != nil {
		return nil
	}
	return rep.Lookup(n.ID)
}

// Resolve turns a node ID or a name into exactly one node. Names that match
// several symbols return an error listing candidates, so the caller can retry
// with an ID instead of guessing.
func (s Service) Resolve(ctx context.Context, repo, idOrName string) (model.Node, error) {
	s.R = store.ForRepository(s.R, repo)
	if strings.Count(idOrName, "|") >= 2 {
		return s.R.Node(ctx, idOrName)
	}
	matches, err := s.R.Search(ctx, repo, idOrName, []model.NodeKind{
		model.KindFunction, model.KindMethod, model.KindInterfaceMethod, model.KindType,
	}, 10)
	if err != nil {
		return model.Node{}, err
	}
	var exact []model.Node
	for _, m := range matches {
		if m.Name == idOrName || m.QualifiedName == idOrName || strings.HasSuffix(m.QualifiedName, "."+idOrName) {
			exact = append(exact, m)
		}
	}
	switch {
	case len(exact) == 1:
		return exact[0], nil
	case len(exact) == 0 && len(matches) == 1:
		return matches[0], nil
	case len(matches) == 0:
		return model.Node{}, fmt.Errorf("symbol %q: %w", idOrName, store.ErrNotFound)
	}
	candidates := exact
	if len(candidates) == 0 {
		candidates = matches
	}
	names := make([]string, 0, len(candidates))
	for _, c := range candidates {
		names = append(names, c.ID)
	}
	return model.Node{}, &AmbiguousError{Query: idOrName, Candidates: names}
}

// AmbiguousError lists the IDs that matched an ambiguous name.
type AmbiguousError struct {
	Query      string
	Candidates []string
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("%q is ambiguous; use one of these ids: %s", e.Query, strings.Join(e.Candidates, ", "))
}

// Card builds a SymbolCard. limit bounds each list.
func (s Service) Card(ctx context.Context, repo, idOrName string, limit int) (SymbolCard, error) {
	s.R = store.ForRepository(s.R, repo)
	limit = store.ClampLimit(limit, 10)
	n, err := s.Resolve(ctx, repo, idOrName)
	if err != nil {
		return SymbolCard{}, err
	}
	card := SymbolCard{Symbol: n}

	callers, err := s.R.Traverse(ctx, n.ID, callEdges, store.In, 1, limit)
	if err != nil {
		return card, err
	}
	card.Callers = refsOf(callers)

	callees, err := s.R.Traverse(ctx, n.ID, []model.EdgeKind{model.EdgeCalls}, store.Out, 1, limit)
	if err != nil {
		return card, err
	}
	card.Callees = refsOf(callees)

	tests, err := s.Tests(ctx, n.ID, 3, limit)
	if err != nil {
		return card, err
	}
	card.Tests = tests

	if n.Kind == model.KindInterfaceMethod {
		impls, err := s.R.Traverse(ctx, n.ID, []model.EdgeKind{model.EdgeDispatchesTo}, store.Out, 1, limit)
		if err != nil {
			return card, err
		}
		card.Implementations = refsOf(impls)
	}

	docs, err := s.R.Traverse(ctx, n.ID, []model.EdgeKind{model.EdgeMentions}, store.In, 1, limit)
	if err != nil {
		return card, err
	}
	card.Docs = refsOf(docs)
	card.Duplicate = DuplicateOf(ctx, s.R, repo, n)
	return card, nil
}

// Tests returns test functions that reach id through calls within depth.
func (s Service) Tests(ctx context.Context, id string, depth, limit int) ([]Ref, error) {
	hits, err := s.R.Traverse(ctx, id, callEdges, store.In, depth, store.MaxLimit)
	if err != nil {
		return nil, err
	}
	var out []Ref
	for _, h := range hits {
		if h.Node.IsTest {
			r := RefOf(h.Node)
			r.Depth = h.Depth
			out = append(out, r)
		}
	}
	return out[:min(len(out), store.ClampLimit(limit, 10))], nil
}

// OutlineEntry is one symbol or section in a file outline.
type OutlineEntry struct {
	ID        string         `json:"id"`
	Kind      model.NodeKind `json:"kind"`
	Name      string         `json:"name"`
	Lines     string         `json:"lines"`
	Signature string         `json:"signature,omitempty"`
	Summary   string         `json:"summary,omitempty"`
	// Level is the heading level for doc sections.
	Level int `json:"level,omitempty"`
}

// Outline lists what a file contains with line ranges, so an agent can read
// only the lines it needs.
func (s Service) Outline(ctx context.Context, repo, file string) ([]OutlineEntry, error) {
	s.R = store.ForRepository(s.R, repo)
	ns, err := s.R.NodesInFile(ctx, repo, strings.TrimPrefix(file, "./"))
	if err != nil {
		return nil, err
	}
	out := make([]OutlineEntry, 0, len(ns))
	for _, n := range ns {
		out = append(out, OutlineEntry{
			ID: n.ID, Kind: n.Kind, Name: n.ShortName(), Signature: n.Signature, Summary: n.Summary, Level: n.Level,
			Lines: fmt.Sprintf("%d-%d", n.StartLine, max(n.StartLine, n.EndLine)),
		})
	}
	return out, nil
}

// SymbolsAt returns the code symbols whose line range overlaps [start, end].
func (s Service) SymbolsAt(ctx context.Context, repo, file string, start, end int) ([]model.Node, error) {
	s.R = store.ForRepository(s.R, repo)
	ns, err := s.R.NodesInFile(ctx, repo, file)
	if err != nil {
		return nil, err
	}
	var out []model.Node
	for _, n := range ns {
		if n.Kind == model.KindSection {
			continue
		}
		if n.StartLine <= end && max(n.StartLine, n.EndLine) >= start {
			out = append(out, n)
		}
	}
	// A method inside a type's range should not hide behind the type; keep
	// the narrowest symbols first.
	slices.SortStableFunc(out, func(a, b model.Node) int {
		return cmp.Compare(a.EndLine-a.StartLine, b.EndLine-b.StartLine)
	})
	return out, nil
}

// Overview is a compact map of a repository for an agent's first look.
type Overview struct {
	Repo     model.Node  `json:"repo"`
	Stats    model.Stats `json:"stats"`
	Packages []Ref       `json:"packages"`
	Docs     []Ref       `json:"docs"`
}

// Overview summarises a repository: stats, packages and documents.
func (s Service) Overview(ctx context.Context, repo string) (Overview, error) {
	s.R = store.ForRepository(s.R, repo)
	st, err := s.R.Stats(ctx, repo)
	if err != nil {
		return Overview{}, err
	}
	ov := Overview{Stats: st}
	if n, err := s.R.Node(ctx, model.NodeID(repo, model.KindRepository, repo)); err == nil {
		ov.Repo = n
	}
	pkgs, err := s.R.List(ctx, repo, store.Filter{Kinds: []model.NodeKind{model.KindPackage}}, store.ByName, 200)
	if err != nil {
		return ov, err
	}
	for _, p := range pkgs {
		ov.Packages = append(ov.Packages, RefOf(p.Node))
	}
	docs, err := s.R.List(ctx, repo, store.Filter{Kinds: []model.NodeKind{model.KindDocument}}, store.ByName, 100)
	if err != nil {
		return ov, err
	}
	for _, d := range docs {
		ov.Docs = append(ov.Docs, RefOf(d.Node))
	}
	return ov, nil
}

// Hotspot is a function ranked by complexity weighted by change frequency.
type Hotspot struct {
	Ref
	Complexity int      `json:"complexity"`
	FileChurn  int      `json:"file_churn"`
	FanIn      int      `json:"fan_in"`
	Score      int      `json:"score"`
	Tags       []string `json:"security_tags,omitempty"`
}

// Hotspots ranks non-test functions by complexity x (1 + churn of their
// file). Complex code that changes often is where defects concentrate.
func (s Service) Hotspots(ctx context.Context, repo string, limit int) ([]Hotspot, error) {
	s.R = store.ForRepository(s.R, repo)
	limit = store.ClampLimit(limit, 20)
	fns, err := s.R.List(ctx, repo, store.Filter{
		Kinds: []model.NodeKind{model.KindFunction, model.KindMethod}, ExcludeTests: true,
	}, store.ByComplexity, store.MaxLimit)
	if err != nil {
		return nil, err
	}
	files, err := s.R.List(ctx, repo, store.Filter{Kinds: []model.NodeKind{model.KindFile}}, store.ByChurn, store.MaxLimit)
	if err != nil {
		return nil, err
	}
	churn := map[string]int{}
	for _, f := range files {
		churn[f.Node.File] = f.Score
	}
	fanIn, err := s.R.List(ctx, repo, store.Filter{
		Kinds: []model.NodeKind{model.KindFunction, model.KindMethod}, ExcludeTests: true,
	}, store.ByFanIn, store.MaxLimit)
	if err != nil {
		return nil, err
	}
	fan := map[string]int{}
	for _, f := range fanIn {
		fan[f.Node.ID] = f.Score
	}
	out := make([]Hotspot, 0, len(fns))
	for _, f := range fns {
		c := churn[f.Node.File]
		out = append(out, Hotspot{
			Ref: RefOf(f.Node), Complexity: f.Node.Complexity, FileChurn: c, FanIn: fan[f.Node.ID],
			Score: f.Node.Complexity * (1 + c), Tags: f.Node.SecurityTags,
		})
	}
	slices.SortStableFunc(out, func(a, b Hotspot) int { return cmp.Compare(b.Score, a.Score) })
	return out[:min(len(out), limit)], nil
}
