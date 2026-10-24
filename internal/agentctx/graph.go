package agentctx

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/review"
	"github.com/astrona-io/codesynapse/internal/store"
)

// RefList is a bounded list with its untruncated total.
type RefList struct {
	Total   int   `json:"total"`
	Results []Ref `json:"results"`
}

// Implementations resolves interface dispatch without guessing types:
//
//	interface type   -> concrete types implementing it
//	concrete type    -> interfaces it implements
//	interface method -> concrete methods it dispatches to
//	concrete method  -> interface methods it satisfies
func (s Service) Implementations(ctx context.Context, repo, symbol string, limit int) (RefList, error) {
	s.R = store.ForRepository(s.R, repo)
	n, err := s.Resolve(ctx, repo, symbol)
	if err != nil {
		return RefList{}, err
	}
	var edge model.EdgeKind
	dir := store.Out
	switch n.Kind {
	case model.KindType:
		edge = model.EdgeImplements
		if n.Signature == "interface" {
			dir = store.In
		}
	case model.KindInterfaceMethod:
		edge = model.EdgeDispatchesTo
	case model.KindMethod:
		edge, dir = model.EdgeDispatchesTo, store.In
	default:
		return RefList{Results: []Ref{}}, nil
	}
	hits, err := s.R.Traverse(ctx, n.ID, []model.EdgeKind{edge}, dir, 1, store.MaxLimit)
	if err != nil {
		return RefList{}, err
	}
	return RefList{Total: len(hits), Results: refs(hits, store.ClampLimit(limit, 50), false)}, nil
}

// CallGraph is a bounded call neighbourhood with its edges.
type CallGraph struct {
	Budget  *ResponseBudget `json:"budget,omitempty"`
	Root    Symbol          `json:"root"`
	Nodes   []Ref           `json:"nodes"`
	Edges   []model.Edge    `json:"edges"`
	Total   int             `json:"total"`
	Entries []string        `json:"entry_points,omitempty"`
}

// CallGraph returns callers ("callers"), callees ("callees") or both up to
// depth (default 2, max 4). Edge endpoints use qualified IDs.
func (s Service) CallGraph(ctx context.Context, repo, symbol, direction string, depth, limit int, maxTokens ...int) (CallGraph, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	s.R = store.ForRepository(s.R, repo)
	n, err := s.Resolve(ctx, repo, symbol)
	if err != nil {
		return CallGraph{}, err
	}
	if depth <= 0 {
		depth = 2
	}
	depth = min(depth, 4)
	limit = store.ClampLimit(limit, 60)
	var hits []model.Hit
	if direction != "callees" {
		in, err := s.R.Traverse(ctx, n.ID, callEdges, store.In, depth, limit)
		if err != nil {
			return CallGraph{}, err
		}
		hits = append(hits, in...)
	}
	if direction != "callers" {
		out, err := s.R.Traverse(ctx, n.ID, []model.EdgeKind{model.EdgeCalls}, store.Out, depth, limit)
		if err != nil {
			return CallGraph{}, err
		}
		hits = append(hits, out...)
	}
	g := CallGraph{Root: symbolOf(n), Total: len(hits)}
	ids := []string{n.ID}
	names := map[string]string{n.ID: n.ShortName()}
	seen := map[string]bool{n.ID: true}
	for _, h := range hits {
		if seen[h.Node.ID] {
			continue
		}
		seen[h.Node.ID] = true
		ids = append(ids, h.Node.ID)
		names[h.Node.ID] = h.Node.ShortName()
		r := refOf(h.Node, h.Depth, false)
		g.Nodes = append(g.Nodes, r)
		if r.EntryPoint && direction != "callees" {
			g.Entries = append(g.Entries, r.QualifiedID)
		}
	}
	edges, err := s.R.Subgraph(ctx, ids, callEdges)
	if err != nil {
		return g, err
	}
	for _, e := range edges {
		if names[e.From] == "" || names[e.To] == "" {
			continue
		}
		e.From, e.To = names[e.From], names[e.To]
		e.Evidence = e.Evidence.OrUnknown()
		g.Edges = append(g.Edges, e)
	}
	if g.Nodes == nil {
		g.Nodes = []Ref{}
	}
	budget := 0
	if len(maxTokens) > 0 {
		budget = maxTokens[0]
	}
	return finishCallGraph(g, budget)
}

// PRContext is get_pr_context: the review report plus the questions the
// guide asks about a change, answered directly.
type PRContext struct {
	review.Report
	Budget           *ResponseBudget `json:"budget,omitempty"`
	ExportedChanged  []string        `json:"exported_changed"`
	ExportedUntested []string        `json:"exported_untested"`
	SensitiveTouched []string        `json:"sensitive_touched"`
	// DuplicatedChanged names changed functions that have copies, with where
	// the copies are: the same change probably belongs there too.
	DuplicatedChanged []string `json:"duplicated_changed"`
}

// PROptions select the change to analyse: a unified diff, a list of files
// (every symbol in them counts as changed), or base/head refs with root.
type PROptions struct {
	Diff      []byte
	Files     []string
	Base      string
	Head      string
	Root      string
	Depth     int
	MaxTokens int
	// BaseGraph, when set, is a graph of the change's base: the answer then
	// says which findings the change introduces and fixes (BaseLabel names
	// the base, e.g. a commit).
	BaseGraph store.Reader
	BaseLabel string
}

// PRContext analyses a change.
func (s Service) PRContext(ctx context.Context, repo string, o PROptions) (PRContext, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if len(repo) > 4096 || len(o.Diff) > 4<<20 || len(o.Files) > 100 {
		return PRContext{}, errors.New("context accepts at most 4 MiB of diff or 100 files")
	}
	for _, file := range o.Files {
		if len(file) > 4096 {
			return PRContext{}, errors.New("file path exceeds 4096 bytes")
		}
	}
	s.R = store.ForRepository(s.R, repo)
	var changes []gitx.FileChange
	switch {
	case len(o.Diff) > 0:
		c, err := gitx.ParseDiff(o.Diff)
		if err != nil {
			return PRContext{}, err
		}
		changes = c
	case len(o.Files) > 0:
		for _, f := range o.Files {
			f = strings.TrimPrefix(strings.TrimSpace(f), "./")
			if f != "" {
				changes = append(changes, gitx.FileChange{Path: f, Ranges: []gitx.LineRange{{Start: 1, End: math.MaxInt32}}})
			}
		}
	case o.Base != "":
		head := o.Head
		if head == "" {
			head = "HEAD"
		}
		d, err := gitx.Diff(ctx, o.Root, o.Base, head)
		if err != nil {
			return PRContext{}, err
		}
		c, err := gitx.ParseDiff(d)
		if err != nil {
			return PRContext{}, err
		}
		changes = c
	default:
		return PRContext{}, errDiffRequired
	}
	if len(changes) > 100 {
		return PRContext{}, errors.New("context accepts at most 100 changed files; narrow the diff")
	}
	depth := o.Depth
	if depth <= 0 {
		depth = 3
	}
	rep, err := review.Analyze(ctx, s.q(), repo, changes, review.Options{Depth: min(depth, 4)})
	if err != nil {
		return PRContext{}, err
	}
	if o.BaseGraph != nil {
		files := make([]string, 0, len(changes))
		for _, c := range changes {
			files = append(files, c.Path)
		}
		d, err := review.CompareFindings(ctx, store.ForRepository(o.BaseGraph, repo), s.R, repo, files)
		if err != nil {
			return PRContext{}, fmt.Errorf("comparing findings with the base: %w", err)
		}
		d.Base = o.BaseLabel
		rep.Security = &d
	}
	out := PRContext{Report: rep, ExportedChanged: []string{}, ExportedUntested: []string{}, SensitiveTouched: []string{}, DuplicatedChanged: []string{}}
	out.Summary.DuplicatedChanges = 0
	for i := range out.Changes {
		c := &out.Changes[i]
		c.Duplicate = s.scopedDuplicate(ctx, c.Duplicate)
		n, err := s.R.Node(ctx, c.ID)
		if err != nil {
			continue
		}
		if d := c.Duplicate; d != nil {
			out.Summary.DuplicatedChanges++
			where := make([]string, 0, len(d.Others))
			for _, o := range d.Others {
				where = append(where, fmt.Sprintf("%s:%d", o.File, o.StartLine))
			}
			label := "exact copies"
			if d.Kind == "near" {
				label = fmt.Sprintf("edited copies ~%.0f%% alike", d.Similarity*100)
			}
			out.DuplicatedChanged = append(out.DuplicatedChanged, fmt.Sprintf("%s [%s: %s]", c.Name, label, strings.Join(where, ", ")))
		}
		if n.Exported {
			out.ExportedChanged = append(out.ExportedChanged, c.Name)
			if len(c.Tests) == 0 && n.Kind.IsCallable() {
				out.ExportedUntested = append(out.ExportedUntested, c.Name)
			}
		}
		if len(c.Tags) > 0 || len(c.Findings) > 0 {
			out.SensitiveTouched = append(out.SensitiveTouched, c.Name+" ["+strings.Join(c.Tags, ",")+"]")
		}
	}
	if f, ok, stale, err := s.runtimeFacts(ctx, repo); err != nil {
		return PRContext{}, err
	} else if ok {
		out.Measured = review.MeasuredOf(f, stale, out.Report)
	}
	return finishPR(out, o.MaxTokens)
}

var errDiffRequired = errorString("pass diff, files, or base (and optionally head)")

type errorString string

func (e errorString) Error() string { return string(e) }

// MinimalItem is one entry of a minimal context pack.
type MinimalItem struct {
	QualifiedID   string         `json:"qualified_id"`
	Kind          model.NodeKind `json:"kind"`
	Read          string         `json:"read,omitempty"`
	Signature     string         `json:"signature,omitempty"`
	Summary       string         `json:"summary,omitempty"`
	Why           string         `json:"why"`
	Revision      string         `json:"revision,omitempty"`
	Retrieval     string         `json:"retrieval"`
	DetailOmitted bool           `json:"detail_omitted,omitempty"`
}

// MinimalContext is the token-budgeted answer to "what should I look at
// for this task".
type MinimalContext struct {
	Task       string   `json:"task"`
	Budget     int      `json:"max_tokens"`
	UsedTokens int      `json:"used_tokens"`
	Keywords   []string `json:"keywords"`
	// Revision is the commit every item was indexed at, unless an item
	// names its own.
	Revision string `json:"revision,omitempty"`
	// Conventions are the repository's house style (how tests assert,
	// errors, logging, frameworks), when asked for: on top of the budget,
	// which the task's matches keep.
	Conventions []string      `json:"conventions,omitempty"`
	Items       []MinimalItem `json:"items"`
	Omitted     int           `json:"omitted"`
	Next        string        `json:"next"`
	Estimator   string        `json:"estimator"`
	Source      string        `json:"source"`
}

var (
	identRe   = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_./()*:-]*`)
	stopwords = map[string]bool{
		"the": true, "and": true, "for": true, "with": true, "that": true, "this": true, "from": true, "into": true,
		"when": true, "what": true, "where": true, "which": true, "how": true, "why": true, "add": true, "fix": true,
		"make": true, "use": true, "using": true, "should": true, "need": true, "want": true, "all": true, "any": true,
		"not": true, "are": true, "can": true, "get": true, "set": true, "new": true, "change": true, "update": true,
		"code": true, "function": true, "method": true, "file": true, "test": true, "tests": true, "bug": true,
	}
)

// keywords extracts identifier-like terms, splitting CamelCase and
// snake_case so "ReplaceRepo" also searches "replace" and "repo".
func keywords(task string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(w string) {
		lw := strings.ToLower(strings.Trim(w, "./()*"))
		if lw == "" || seen[lw] || ((len(lw) < 3 || stopwords[lw]) && w == strings.ToLower(w)) {
			return
		}
		seen[lw] = true
		out = append(out, strings.Trim(w, "./()*"))
	}
	words := identRe.FindAllString(task, -1)
	// Whole identifiers precede split fragments so an early CamelCase term
	// cannot consume the keyword cap and hide a later exact identifier.
	for _, w := range words {
		add(w)
		// Preserve punctuation in identifiers, but also accept a prose colon
		// or dash following a symbol: "Handler: fix nil checks".
		add(strings.TrimRight(w, ":-"))
	}
	for _, w := range words {
		if i := strings.LastIndexAny(w, "./"); i >= 0 {
			add(w[i+1:])
		}
		for _, part := range splitIdent(w) {
			add(part)
		}
	}
	return out[:min(len(out), 12)]
}

func splitIdent(w string) []string {
	var parts []string
	var cur []rune
	rs := []rune(w)
	for i, r := range rs {
		boundary := r == '_' || r == '.' || r == '/' ||
			(i > 0 && unicode.IsUpper(r) && (unicode.IsLower(rs[i-1]) || (i+1 < len(rs) && unicode.IsLower(rs[i+1]))))
		if boundary && len(cur) > 0 {
			parts = append(parts, string(cur))
			cur = nil
		}
		if r != '_' && r != '.' && r != '/' {
			cur = append(cur, r)
		}
	}
	if len(cur) > 0 {
		parts = append(parts, string(cur))
	}
	if len(parts) <= 1 {
		return nil
	}
	return parts
}

// MinimalContext ranks symbols, doc sections and config entries for a task
// and packs the best ones until maxTokens (estimated at bytes/4) is spent.
func (s Service) MinimalContext(ctx context.Context, repo, task string, files []string, maxTokens int) (MinimalContext, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if len(task) > 8192 || len(repo) > 4096 || len(files) > 100 {
		return MinimalContext{}, errors.New("context accepts a task up to 8192 bytes, a repository up to 4096 bytes, and at most 100 files")
	}
	for _, file := range files {
		if len(file) > 4096 {
			return MinimalContext{}, errors.New("file path exceeds 4096 bytes")
		}
	}
	s.R = store.ForRepository(s.R, repo)
	maxTokens = contextBudget(maxTokens, 1500)
	mc := MinimalContext{Task: task, Budget: maxTokens, Keywords: keywords(task), Items: []MinimalItem{}, Estimator: JSONEstimator, Source: "indexed_graph"}
	inFiles := map[string]bool{}
	for _, f := range files {
		inFiles[strings.TrimPrefix(f, "./")] = true
	}
	type scored struct {
		n     model.Node
		score float64
		why   []string
		exact bool
	}
	byID := map[string]*scored{}
	exactTerms := map[string]bool{}
	for _, word := range identRe.FindAllString(task, -1) {
		word = strings.Trim(word, "./")
		exactTerms[word] = true
		exactTerms[strings.TrimRight(word, ":-")] = true
	}
	// Files too: a task often names a file or area (Dockerfile, a chart, a
	// page) before any symbol in it, and the task eval finds more with them.
	kinds := []model.NodeKind{model.KindFunction, model.KindMethod, model.KindInterfaceMethod, model.KindType, model.KindSection, model.KindConfigEntry, model.KindTemplateDef, model.KindFile}
	for i, kw := range mc.Keywords {
		if err := ctx.Err(); err != nil {
			return MinimalContext{}, err
		}
		hits, err := s.R.Search(ctx, repo, kw, kinds, 60)
		if err != nil {
			return mc, err
		}
		if exactTerms[kw] && strings.ContainsAny(kw, "./") {
			for _, kind := range kinds {
				n, err := s.R.Node(ctx, model.NodeID(repo, kind, kw))
				if err == nil {
					hits = append(hits, n)
				} else if !errors.Is(err, store.ErrNotFound) {
					return MinimalContext{}, err
				}
			}
		}
		weight := 1.0 / (1 + float64(i)*0.15) // earlier keywords matter more
		seen := map[string]bool{}
		for _, n := range hits {
			if seen[n.ID] {
				continue
			}
			seen[n.ID] = true
			sc := byID[n.ID]
			if sc == nil {
				sc = &scored{n: n}
				byID[n.ID] = sc
			}
			if exactTerms[kw] && (n.Name == kw || n.ShortName() == kw || n.QualifiedName == kw) {
				sc.exact = true
			}
			switch {
			case strings.EqualFold(n.Name, kw) || strings.EqualFold(n.ShortName(), kw):
				sc.score += 3 * weight
			default:
				sc.score += weight
			}
			sc.why = append(sc.why, kw)
		}
	}
	for _, sc := range byID {
		if inFiles[sc.n.File] {
			sc.score += 2
			sc.why = append(sc.why, "in listed file")
		}
		if sc.n.IsTest {
			sc.score *= 0.5
		}
		if sc.n.Exported {
			sc.score += 0.3
		}
	}
	ranked := make([]*scored, 0, len(byID))
	for _, sc := range byID {
		ranked = append(ranked, sc)
	}
	slices.SortFunc(ranked, func(a, b *scored) int {
		if a.exact != b.exact {
			if a.exact {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(b.score, a.score), cmp.Compare(len(a.n.ID), len(b.n.ID)), cmp.Compare(a.n.ID, b.n.ID))
	})

	mc.Omitted = len(ranked)
	mc.Next = "Read listed ranges; use get_symbol_context before editing. Omitted matches may require a narrower task or larger max_tokens."
	if len(ranked) == 0 {
		mc.Next = "Nothing matched; try search with a symbol name or repo_overview."
	}
	if n, err := measuredTokens(&mc, &mc.UsedTokens); err != nil {
		return MinimalContext{}, err
	} else if n > maxTokens {
		return MinimalContext{}, ErrResponseBudget
	}
	for _, sc := range ranked {
		item := MinimalItem{
			QualifiedID: sc.n.ShortName(), Kind: sc.n.Kind, Read: sc.n.Location(),
			Signature: sc.n.Signature, Summary: sc.n.Summary, Why: "matches " + strings.Join(dedupe(sc.why), ", "),
			Revision: sc.n.Revision, Retrieval: "lexical",
		}
		if sc.exact {
			item.Retrieval = "exact"
		}
		if mc.Revision == "" {
			mc.Revision = item.Revision
		}
		if item.Revision == mc.Revision {
			item.Revision = "" // said once at the top
		}
		mc.Items = append(mc.Items, item)
		mc.Omitted--
		for {
			n, err := measuredTokens(&mc, &mc.UsedTokens)
			if err != nil {
				return MinimalContext{}, err
			}
			if n <= maxTokens {
				break
			}
			last := &mc.Items[len(mc.Items)-1]
			if last.Summary != "" || last.Signature != "" {
				last.Summary, last.Signature, last.DetailOmitted = "", "", true
				continue
			}
			mc.Items = mc.Items[:len(mc.Items)-1]
			mc.Omitted++
			break
		}
	}
	if _, err := measuredTokens(&mc, &mc.UsedTokens); err != nil {
		return MinimalContext{}, err
	}
	return mc, nil
}

func dedupe(in []string) []string {
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

// PackageCtx is the package view of get_symbol_context.
type PackageCtx struct {
	Path        string   `json:"path"`
	Summary     string   `json:"summary,omitempty"`
	Files       int      `json:"files"`
	Exported    []Ref    `json:"exported"`
	Interfaces  []Ref    `json:"interfaces"`
	Hubs        []Ref    `json:"hubs"`
	Imports     []string `json:"imports"`
	ImportedBy  []string `json:"imported_by"`
	EntryPoints []Ref    `json:"entry_points,omitempty"`
}

func (s Service) packageContext(ctx context.Context, repo string, pkg model.Node, limit int) (PackageCtx, error) {
	s.R = store.ForRepository(s.R, repo)
	pc := PackageCtx{Path: pkg.QualifiedName, Summary: pkg.Summary, Exported: []Ref{}, Interfaces: []Ref{}, Hubs: []Ref{}, Imports: []string{}, ImportedBy: []string{}}
	members, err := s.R.Traverse(ctx, pkg.ID, []model.EdgeKind{model.EdgeContains, model.EdgeDefines}, store.Out, 2, store.MaxLimit)
	if err != nil {
		return pc, err
	}
	fanIn, err := s.R.List(ctx, repo, store.Filter{Kinds: []model.NodeKind{model.KindFunction, model.KindMethod}, ExcludeTests: true}, store.ByFanIn, store.MaxLimit)
	if err != nil {
		return pc, err
	}
	score := map[string]int{}
	for _, r := range fanIn {
		score[r.Node.ID] = r.Score
	}
	var hubs []model.Node
	for _, m := range members {
		n := m.Node
		switch {
		case n.Kind == model.KindFile:
			pc.Files++
		case n.IsTest:
		case n.Kind == model.KindType && n.Signature == "interface":
			pc.Interfaces = append(pc.Interfaces, refOf(n, 0, true))
		case n.Exported && (n.Kind == model.KindType || n.Kind == model.KindFunction):
			pc.Exported = append(pc.Exported, refOf(n, 0, true))
		}
		if n.Kind.IsCallable() && !n.IsTest {
			hubs = append(hubs, n)
			if n.Name == "main" || (n.Exported && n.Kind == model.KindFunction && strings.HasPrefix(n.Name, "New")) {
				pc.EntryPoints = append(pc.EntryPoints, refOf(n, 0, false))
			}
		}
	}
	slices.SortFunc(hubs, func(a, b model.Node) int { return cmp.Compare(score[b.ID], score[a.ID]) })
	for _, h := range hubs[:min(len(hubs), limit)] {
		if score[h.ID] == 0 {
			break
		}
		r := refOf(h, 0, false)
		r.Summary = strconv.Itoa(score[h.ID]) + " callers"
		pc.Hubs = append(pc.Hubs, r)
	}
	pc.Exported = pc.Exported[:min(len(pc.Exported), limit*3)]
	for dir, dst := range map[store.Direction]*[]string{store.Out: &pc.Imports, store.In: &pc.ImportedBy} {
		hits, err := s.R.Traverse(ctx, pkg.ID, []model.EdgeKind{model.EdgeImports}, dir, 1, 100)
		if err != nil {
			return pc, err
		}
		for _, h := range hits {
			*dst = append(*dst, h.Node.QualifiedName)
		}
		slices.Sort(*dst)
	}
	return pc, nil
}
