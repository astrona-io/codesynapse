// Package metrics computes repository-wide code, test, doc and coupling
// statistics plus a token benchmark from a complete in-memory graph. It runs
// once per ingest, where the whole graph is at hand and exact answers are
// cheap; the result is stored on the Repository node.
package metrics

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/query"
	"github.com/astrona-io/codesynapse/internal/store/memory"
)

// testReachDepth is how many call hops a test may be away from a function and
// still count as reaching it.
const testReachDepth = 3

// Report is the full metrics output.
type Report struct {
	GeneratedAt   string                  `json:"generated_at"`
	Code          Code                    `json:"code"`
	Tests         Tests                   `json:"tests"`
	Docs          Docs                    `json:"docs"`
	Distributions map[string]Distribution `json:"distributions"`
	Packages      []Package               `json:"packages"`
	DeadCode      []query.Ref             `json:"dead_code"`
	Benchmark     Benchmark               `json:"benchmark"`
}

// Code holds size and structure counts.
type Code struct {
	Packages        int     `json:"packages"`
	Files           int     `json:"files"`
	LOC             int     `json:"loc"`
	TestLOC         int     `json:"test_loc"`
	Functions       int     `json:"functions"`
	Methods         int     `json:"methods"`
	Types           int     `json:"types"`
	Interfaces      int     `json:"interfaces"`
	Exported        int     `json:"exported"`
	CallEdges       int     `json:"call_edges"`
	AvgComplexity   float64 `json:"avg_complexity"`
	ComplexOver10   int     `json:"complex_over_10"`
	ComplexOver20   int     `json:"complex_over_20"`
	SecuritySymbols int     `json:"security_symbols"`
}

// Tests holds test-reachability metrics. "Reached" means a test function
// calls the symbol within testReachDepth hops; it is a structural proxy for
// coverage, not measured coverage.
type Tests struct {
	TestFunctions    int         `json:"test_functions"`
	TestToCodeRatio  float64     `json:"test_to_code_ratio"`
	Reachable        int         `json:"reachable"`
	Total            int         `json:"total"`
	ReachablePct     float64     `json:"reachable_pct"`
	ExportedUntested int         `json:"exported_untested"`
	UntestedComplex  []query.Ref `json:"untested_complex"`
}

// Docs holds documentation metrics.
type Docs struct {
	Exported         int     `json:"exported"`
	Documented       int     `json:"documented"`
	CoveragePct      float64 `json:"coverage_pct"`
	Documents        int     `json:"documents"`
	Sections         int     `json:"sections"`
	Mentions         int     `json:"mentions"`
	SymbolsMentioned int     `json:"symbols_mentioned"`
}

// Distribution summarises one integer metric over a node population.
type Distribution struct {
	Count   int      `json:"count"`
	Min     int      `json:"min"`
	Max     int      `json:"max"`
	Mean    float64  `json:"mean"`
	P50     int      `json:"p50"`
	P90     int      `json:"p90"`
	P99     int      `json:"p99"`
	Buckets []Bucket `json:"buckets"`
}

// Bucket is one histogram bar.
type Bucket struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// Package holds per-package coupling metrics (Robert C. Martin):
// Ca = packages importing this one, Ce = packages it imports,
// instability I = Ce / (Ca + Ce), from 0 (stable) to 1 (unstable).
type Package struct {
	Name          string  `json:"name"`
	Files         int     `json:"files"`
	LOC           int     `json:"loc"`
	Functions     int     `json:"functions"`
	Afferent      int     `json:"afferent"`
	Efferent      int     `json:"efferent"`
	Instability   float64 `json:"instability"`
	AvgComplexity float64 `json:"avg_complexity"`
	DocPct        float64 `json:"doc_pct"`
	TestedPct     float64 `json:"tested_pct"`
}

// Benchmark estimates tokens saved by asking the graph instead of reading
// files. For each sampled function, the graph answer is its real
// symbol_card JSON; the baseline is the set of files an agent would open to
// learn the same things: the defining file plus the files of direct callers,
// callees and reaching tests. Tokens are estimated as bytes/4.
type Benchmark struct {
	PolicyDigest   string       `json:"policy_digest,omitempty"`
	Method         string       `json:"method"`
	Samples        int          `json:"samples"`
	GraphTokens    Distribution `json:"graph_tokens"`
	BaselineTokens Distribution `json:"baseline_tokens"`
	MedianSavingsX float64      `json:"median_savings_x"`
	// MedianReductionPct is the median per-question token reduction, and
	// TotalReductionPct the reduction over the whole sample, both in percent:
	// 95 means the graph answer needs 5% of the tokens reading files would.
	MedianReductionPct float64         `json:"median_reduction_pct"`
	TotalReductionPct  float64         `json:"total_reduction_pct"`
	TotalGraph         int             `json:"total_graph_tokens"`
	TotalBaseline      int             `json:"total_baseline_tokens"`
	Examples           []BenchmarkCase `json:"examples"`
}

// BenchmarkCase is one sampled function.
type BenchmarkCase struct {
	Symbol         string  `json:"symbol"`
	GraphTokens    int     `json:"graph_tokens"`
	BaselineTokens int     `json:"baseline_tokens"`
	Files          int     `json:"files"`
	SavingsX       float64 `json:"savings_x"`
	ReductionPct   float64 `json:"reduction_pct"`
}

// Options tunes Compute.
type Options struct {
	// BenchmarkSamples caps the functions sampled for the token benchmark.
	BenchmarkSamples int
}

// Compute builds a Report for repo from g, which must be the complete graph.
func Compute(ctx context.Context, repo string, g *model.Graph, opts Options) (Report, error) {
	if opts.BenchmarkSamples <= 0 {
		opts.BenchmarkSamples = 150
	}
	idx := newIndex(g)
	rep := Report{GeneratedAt: time.Now().UTC().Format(time.RFC3339), Distributions: map[string]Distribution{}}

	reached := idx.testReached()
	var complexity, loc, fanIn, fanOut []int
	var complexitySum, callables int
	for _, n := range idx.nodes {
		switch n.Kind {
		case model.KindPackage:
			rep.Code.Packages++
		case model.KindFile:
			rep.Code.Files++
			if n.IsTest {
				rep.Code.TestLOC += n.LOC
			} else {
				rep.Code.LOC += n.LOC
			}
		case model.KindType:
			rep.Code.Types++
			if n.Signature == "interface" {
				rep.Code.Interfaces++
			}
		case model.KindDocument:
			rep.Docs.Documents++
		case model.KindSection:
			rep.Docs.Sections++
		}
		if isSymbol(n) && n.Exported && !n.IsTest {
			rep.Docs.Exported++
			rep.Code.Exported++
			if n.Summary != "" {
				rep.Docs.Documented++
			}
		}
		if !isCallable(n) {
			continue
		}
		if n.IsTest {
			rep.Tests.TestFunctions++
			continue
		}
		if n.Kind == model.KindFunction {
			rep.Code.Functions++
		} else {
			rep.Code.Methods++
		}
		callables++
		complexitySum += n.Complexity
		complexity = append(complexity, n.Complexity)
		loc = append(loc, n.LOC)
		fanIn = append(fanIn, idx.count(n.ID, model.EdgeCalls, true))
		fanOut = append(fanOut, idx.count(n.ID, model.EdgeCalls, false))
		if n.Complexity > 10 {
			rep.Code.ComplexOver10++
		}
		if n.Complexity > 20 {
			rep.Code.ComplexOver20++
		}
		if len(n.SecurityTags) > 0 {
			rep.Code.SecuritySymbols++
		}
		rep.Tests.Total++
		if reached[n.ID] {
			rep.Tests.Reachable++
		} else if n.Exported {
			rep.Tests.ExportedUntested++
		}
	}
	for _, e := range idx.edges {
		switch e.Kind {
		case model.EdgeCalls:
			rep.Code.CallEdges++
		case model.EdgeMentions:
			rep.Docs.Mentions++
		}
	}
	rep.Docs.SymbolsMentioned = idx.distinctTargets(model.EdgeMentions)
	rep.Code.AvgComplexity = ratio(complexitySum, callables)
	rep.Docs.CoveragePct = pct(rep.Docs.Documented, rep.Docs.Exported)
	rep.Tests.ReachablePct = pct(rep.Tests.Reachable, rep.Tests.Total)
	rep.Tests.TestToCodeRatio = round2(ratio(rep.Code.TestLOC, rep.Code.LOC))
	rep.Tests.UntestedComplex = idx.untestedComplex(reached, 15)

	rep.Distributions["complexity"] = Distribute(complexity, []int{1, 2, 5, 10, 20, 50})
	rep.Distributions["loc"] = Distribute(loc, []int{5, 10, 25, 50, 100, 200})
	rep.Distributions["fan_in"] = Distribute(fanIn, []int{0, 1, 2, 5, 10, 25})
	rep.Distributions["fan_out"] = Distribute(fanOut, []int{0, 1, 2, 5, 10, 25})

	rep.Packages = idx.packages(reached)
	rep.DeadCode = idx.deadCode(50)

	bench, err := benchmark(ctx, repo, g, idx, opts.BenchmarkSamples)
	if err != nil {
		return rep, fmt.Errorf("token benchmark: %w", err)
	}
	rep.Benchmark = bench
	return rep, nil
}

// Encode returns the report as JSON for storage on the Repository node.
func Encode(r Report) (string, error) {
	b, err := json.Marshal(r)
	return string(b), err
}

func isCallable(n model.Node) bool { return n.Kind == model.KindFunction || n.Kind == model.KindMethod }

func isSymbol(n model.Node) bool { return isCallable(n) || n.Kind == model.KindType }

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func pct(a, b int) float64 { return round2(ratio(a, b) * 100) }

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// Distribute summarises values. bounds are inclusive upper bucket limits; a
// final open bucket catches the rest.
func Distribute(values []int, bounds []int) Distribution {
	d := Distribution{Count: len(values)}
	buckets := make([]Bucket, len(bounds)+1)
	lower := math.MinInt
	for i, b := range bounds {
		switch {
		case lower == math.MinInt || lower+1 == b:
			buckets[i].Label = fmt.Sprintf("≤%d", b)
			if lower != math.MinInt {
				buckets[i].Label = fmt.Sprint(b)
			}
		default:
			buckets[i].Label = fmt.Sprintf("%d–%d", lower+1, b)
		}
		lower = b
	}
	buckets[len(bounds)].Label = fmt.Sprintf(">%d", bounds[len(bounds)-1])
	d.Buckets = buckets
	if len(values) == 0 {
		return d
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	sum := 0
	for _, v := range sorted {
		sum += v
		i, _ := slices.BinarySearch(bounds, v)
		d.Buckets[i].Count++
	}
	d.Min, d.Max = sorted[0], sorted[len(sorted)-1]
	d.Mean = round2(float64(sum) / float64(len(sorted)))
	d.P50, d.P90, d.P99 = percentile(sorted, 50), percentile(sorted, 90), percentile(sorted, 99)
	return d
}

// percentile uses the nearest-rank method on sorted values.
func percentile(sorted []int, p int) int {
	rank := int(math.Ceil(float64(p) / 100 * float64(len(sorted))))
	return sorted[max(0, min(len(sorted)-1, rank-1))]
}

// index gives fast adjacency lookups over a graph.
type index struct {
	nodes map[string]model.Node
	edges []model.Edge
	in    map[string][]model.Edge
	out   map[string][]model.Edge
}

func newIndex(g *model.Graph) *index {
	idx := &index{nodes: map[string]model.Node{}, in: map[string][]model.Edge{}, out: map[string][]model.Edge{}}
	for _, n := range g.Nodes() {
		idx.nodes[n.ID] = n
	}
	idx.edges = g.Edges()
	for _, e := range idx.edges {
		idx.out[e.From] = append(idx.out[e.From], e)
		idx.in[e.To] = append(idx.in[e.To], e)
	}
	return idx
}

func (x *index) count(id string, kind model.EdgeKind, incoming bool) int {
	edges := x.out[id]
	if incoming {
		edges = x.in[id]
	}
	c := 0
	for _, e := range edges {
		if e.Kind == kind {
			c++
		}
	}
	return c
}

func (x *index) distinctTargets(kind model.EdgeKind) int {
	seen := map[string]bool{}
	for _, e := range x.edges {
		if e.Kind == kind {
			seen[e.To] = true
		}
	}
	return len(seen)
}

// testReached walks forward from every test over calls and dispatch, which
// is equivalent to asking "is a test within depth hops" for every function,
// but in one pass.
func (x *index) testReached() map[string]bool {
	reached := map[string]bool{}
	for _, n := range x.nodes {
		if !n.IsTest || !isCallable(n) {
			continue
		}
		frontier := []string{n.ID}
		seen := map[string]bool{n.ID: true}
		for d := 0; d < testReachDepth && len(frontier) > 0; d++ {
			var next []string
			for _, cur := range frontier {
				for _, e := range x.out[cur] {
					if (e.Kind != model.EdgeCalls && e.Kind != model.EdgeDispatchesTo) || seen[e.To] {
						continue
					}
					seen[e.To] = true
					reached[e.To] = true
					next = append(next, e.To)
				}
			}
			frontier = next
		}
	}
	return reached
}

func (x *index) untestedComplex(reached map[string]bool, limit int) []query.Ref {
	var cands []model.Node
	for _, n := range x.nodes {
		if isCallable(n) && !n.IsTest && !reached[n.ID] && n.Complexity >= 5 {
			cands = append(cands, n)
		}
	}
	slices.SortFunc(cands, func(a, b model.Node) int {
		return cmp.Or(cmp.Compare(b.Complexity, a.Complexity), cmp.Compare(a.ID, b.ID))
	})
	out := make([]query.Ref, 0, min(limit, len(cands)))
	for _, n := range cands[:min(limit, len(cands))] {
		out = append(out, query.RefOf(n))
	}
	return out
}

// deadCode lists unexported, non-test functions and methods with no static
// callers and no interface dispatch into them. Functions used only as values
// (callbacks, handlers registered by reference) also appear here, so these
// are candidates to check, not verdicts.
func (x *index) deadCode(limit int) []query.Ref {
	var cands []model.Node
	for _, n := range x.nodes {
		if !isCallable(n) || n.IsTest {
			continue
		}
		// Prefer the reachability analysis (package cleanup) when it ran.
		if n.Liveness != "" {
			if n.Liveness == "unreachable" {
				cands = append(cands, n)
			}
			continue
		}
		if n.Exported {
			continue
		}
		switch n.Name {
		case "main", "init":
			continue
		}
		if x.count(n.ID, model.EdgeCalls, true) > 0 || x.count(n.ID, model.EdgeDispatchesTo, true) > 0 {
			continue
		}
		cands = append(cands, n)
	}
	slices.SortFunc(cands, func(a, b model.Node) int { return cmp.Or(cmp.Compare(b.LOC, a.LOC), cmp.Compare(a.ID, b.ID)) })
	out := make([]query.Ref, 0, min(limit, len(cands)))
	for _, n := range cands[:min(limit, len(cands))] {
		out = append(out, query.RefOf(n))
	}
	return out
}

func (x *index) packages(reached map[string]bool) []Package {
	type acc struct {
		Package
		complexity, callables, exported, documented, tested int
	}
	byID := map[string]*acc{}
	for _, n := range x.nodes {
		if n.Kind != model.KindPackage {
			continue
		}
		a := &acc{Package: Package{Name: n.QualifiedName}}
		a.Afferent = x.count(n.ID, model.EdgeImports, true)
		a.Efferent = x.count(n.ID, model.EdgeImports, false)
		if a.Afferent+a.Efferent > 0 {
			a.Instability = round2(ratio(a.Efferent, a.Afferent+a.Efferent))
		}
		byID[n.ID] = a
	}
	pkgOf := map[string]*acc{}
	for _, n := range x.nodes {
		if n.Package == "" {
			continue
		}
		a := byID[model.NodeID(n.Repo, model.KindPackage, n.Package)]
		if a == nil {
			continue
		}
		pkgOf[n.ID] = a
		switch {
		case n.Kind == model.KindFile && !n.IsTest:
			a.Files++
			a.LOC += n.LOC
		case isCallable(n) && !n.IsTest:
			a.Functions++
			a.callables++
			a.complexity += n.Complexity
			if reached[n.ID] {
				a.tested++
			}
		}
		if isSymbol(n) && n.Exported && !n.IsTest {
			a.exported++
			if n.Summary != "" {
				a.documented++
			}
		}
	}
	out := make([]Package, 0, len(byID))
	for _, a := range byID {
		a.AvgComplexity = round2(ratio(a.complexity, a.callables))
		a.DocPct = pct(a.documented, a.exported)
		a.TestedPct = pct(a.tested, a.callables)
		out = append(out, a.Package)
	}
	slices.SortFunc(out, func(a, b Package) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

// benchmark samples functions deterministically and compares real
// symbol_card output with the files an agent would otherwise read.
func benchmark(ctx context.Context, repo string, g *model.Graph, x *index, samples int) (Benchmark, error) {
	b := Benchmark{PolicyDigest: repoconfig.FromContext(ctx).Digest(), Method: "graph = symbol_card JSON; baseline = defining file + files of direct callers, callees and reaching tests; tokens ≈ bytes/4"}
	if b.PolicyDigest == "" {
		if node, ok := g.Node(model.NodeID(repo, model.KindRepository, repo)); ok && node.Meta != "" {
			var metadata struct {
				Policy struct {
					Digest string `json:"digest"`
				} `json:"repository_policy"`
			}
			if err := json.Unmarshal([]byte(node.Meta), &metadata); err != nil {
				return Benchmark{}, fmt.Errorf("decode benchmark policy: %w", err)
			}
			b.PolicyDigest = metadata.Policy.Digest
		}
	}
	fileBytes := map[string]int{}
	for _, n := range x.nodes {
		if n.Kind == model.KindFile {
			fileBytes[n.File] = n.Bytes
		}
	}
	var pool []model.Node
	for _, n := range x.nodes {
		if isCallable(n) && !n.IsTest && fileBytes[n.File] > 0 {
			pool = append(pool, n)
		}
	}
	if len(pool) == 0 {
		return b, nil
	}
	slices.SortFunc(pool, func(a, b model.Node) int { return cmp.Compare(a.ID, b.ID) })
	stepSize := max(1, len(pool)/samples)

	mem := memory.New()
	if err := mem.ReplaceRepo(ctx, repo, g); err != nil {
		return b, err
	}
	svc := query.Service{R: mem}

	var graphTok, baseTok []int
	var cases []BenchmarkCase
	for i := 0; i < len(pool) && len(cases) < samples; i += stepSize {
		n := pool[i]
		card, err := svc.Card(ctx, repo, n.ID, 10)
		if err != nil {
			return b, err
		}
		raw, err := json.Marshal(card)
		if err != nil {
			return b, err
		}
		files := map[string]bool{n.File: true}
		for _, refs := range [][]query.Ref{card.Callers, card.Callees, card.Tests} {
			for _, r := range refs {
				if f, _, ok := strings.Cut(r.Location, ":"); ok && f != "" {
					files[f] = true
				}
			}
		}
		base := 0
		for f := range files {
			base += fileBytes[f]
		}
		gt, bt := tokens(len(raw)), tokens(base)
		graphTok = append(graphTok, gt)
		baseTok = append(baseTok, bt)
		b.TotalGraph += gt
		b.TotalBaseline += bt
		cases = append(cases, BenchmarkCase{
			Symbol: n.ShortName(), GraphTokens: gt, BaselineTokens: bt, Files: len(files),
			SavingsX: round2(ratio(bt, max(1, gt))), ReductionPct: reduction(gt, bt),
		})
	}
	b.Samples = len(cases)
	b.GraphTokens = Distribute(graphTok, []int{100, 250, 500, 1000, 2000, 5000})
	b.BaselineTokens = Distribute(baseTok, []int{500, 1000, 2500, 5000, 10000, 25000})
	ratios := make([]float64, 0, len(cases))
	for _, c := range cases {
		ratios = append(ratios, c.SavingsX)
	}
	slices.Sort(ratios)
	b.MedianSavingsX = ratios[len(ratios)/2]
	reductions := make([]float64, 0, len(cases))
	for _, c := range cases {
		reductions = append(reductions, c.ReductionPct)
	}
	slices.Sort(reductions)
	b.MedianReductionPct = reductions[len(reductions)/2]
	b.TotalReductionPct = reduction(b.TotalGraph, b.TotalBaseline)
	slices.SortFunc(cases, func(a, b BenchmarkCase) int { return cmp.Compare(b.BaselineTokens, a.BaselineTokens) })
	b.Examples = cases[:min(8, len(cases))]
	return b, nil
}

func tokens(bytes int) int { return (bytes + 3) / 4 }

// reduction is how much smaller graph is than baseline, in percent (one
// decimal). A graph answer larger than the files reports 0, not a negative.
func reduction(graph, baseline int) float64 {
	if baseline <= 0 {
		return 0
	}
	return math.Max(0, math.Round((1-float64(graph)/float64(baseline))*1000)/10)
}
