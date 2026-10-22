package catalog

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/parisnakitakejser/codesynapse/internal/curation"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/query"
	"github.com/parisnakitakejser/codesynapse/internal/store"
)

// Task describes the work to find applicable records for.
type Task struct {
	// Query is the task in words.
	Query string
	// Files and Symbols are what the work touches; Components names parts of
	// the system directly.
	Files      []string
	Symbols    []string
	Components []string
	Limit      int
}

// Applicable is a record that bears on a task and why.
type Applicable struct {
	Entry
	Why   []string `json:"why"`
	Score int      `json:"score"`
}

// ContextResult separates the rules in force from everything else that is
// relevant, so a draft or an RFC is never read as a requirement.
type ContextResult struct {
	// Binding are accepted ADRs and active standards and policies, not superseded.
	Binding []Applicable `json:"binding"`
	// Informational are overviews, runbooks, diagrams and RFCs, and records not in force
	// (proposals, drafts, rejected, superseded): useful background, not rules.
	Informational []Applicable `json:"informational"`
	// Issues are problems with the records involved, such as possible conflicts.
	Issues []Issue  `json:"issues"`
	Notes  []string `json:"notes"`
	// OpenProposals are drafts someone has proposed that match the task. They
	// are not decisions; they are listed so work is not duplicated or contradicted.
	OpenProposals []ProposalView `json:"open_proposals"`
	// Truncated is set when more records applied than Limit allowed; Omitted counts them.
	Truncated bool   `json:"truncated"`
	Omitted   int    `json:"omitted"`
	Source    Source `json:"source"`
}

// Context finds the records that apply to a task: those that name the code it
// touches (its symbols, files and their packages), those for the components it
// names, and those whose words match the task. It reports both what binds and
// what only informs, never merging the two.
func (s Service) Context(ctx context.Context, repo string, t Task) (ContextResult, error) {
	l, err := s.load(ctx, repo)
	if err != nil {
		return ContextResult{}, err
	}
	res := ContextResult{Binding: []Applicable{}, Informational: []Applicable{}, Issues: []Issue{}, Notes: []string{}, OpenProposals: []ProposalView{}, Source: l.source}
	byNode := map[string]string{} // record node ID -> record ID
	for id, n := range l.nodes {
		byNode[n.ID] = id
	}
	why := map[string][]string{}
	score := map[string]int{}
	note := func(id string, points int, reason string) {
		why[id] = append(why[id], reason)
		score[id] += points
	}

	// Code the task touches, and the files and packages around it: a decision
	// about a package covers the symbols in it.
	var touched []model.Node
	for _, f := range t.Files {
		f = strings.TrimPrefix(strings.TrimSpace(f), "./")
		if f == "" {
			continue
		}
		n, err := s.R.Node(ctx, model.NodeID(repo, model.KindFile, f))
		if err != nil {
			res.Notes = append(res.Notes, fmt.Sprintf("file %q is not in the index", f))
			continue
		}
		touched = append(touched, n)
	}
	svc := query.Service{R: s.R}
	for _, sym := range t.Symbols {
		if strings.TrimSpace(sym) == "" {
			continue
		}
		n, err := svc.Resolve(ctx, repo, sym)
		if err != nil {
			res.Notes = append(res.Notes, fmt.Sprintf("symbol %q: %v", sym, err))
			continue
		}
		touched = append(touched, n)
	}
	around := map[string]string{} // node ID -> what it is, for the code the task touches and what surrounds it
	for _, n := range touched {
		around[n.ID] = n.ShortName()
		if n.File != "" && n.Kind != model.KindFile {
			around[model.NodeID(repo, model.KindFile, n.File)] = n.File
		}
		if n.Package != "" && n.Kind != model.KindPackage {
			around[model.NodeID(repo, model.KindPackage, n.Package)] = "package " + n.Package
		}
		s.decisionsFor(ctx, repo, n, byNode, note)
		if n.Kind == model.KindFile {
			inFile, err := s.decisionsInFile(ctx, repo, n, byNode, note)
			if err != nil {
				return res, err
			}
			for id, name := range inFile {
				around[id] = name + " in " + n.File
			}
		}
	}
	// Links added through the catalog count like declared ones, and say who added them.
	for i, it := range l.links {
		if i >= maxLinks {
			res.Notes = append(res.Notes, "more links exist than were checked")
			break
		}
		if k := it.Link; k != nil && k.Kind == curation.Implements {
			if target, err := s.resolveCode(ctx, repo, k.Target); err == nil {
				if what, ok := around[target.ID]; ok {
					note(canonical(l, k.Record), 4, fmt.Sprintf("linked by %s to %s", cmp.Or(it.Client, it.CreatedBy, "a user"), what))
				}
			}
		}
	}
	for _, c := range t.Components {
		c = strings.ToLower(strings.TrimSpace(c))
		for _, id := range l.order {
			if slices.Contains(l.entry(id, nil).Components, c) {
				note(id, 3, fmt.Sprintf("applies to component %s", c))
			}
		}
	}
	if words := strings.Fields(strings.ToLower(t.Query)); len(words) > 0 {
		for _, id := range l.order {
			e := l.entry(id, nil)
			hay := strings.ToLower(strings.Join([]string{e.Title, e.Summary, e.Topic, strings.Join(e.Components, " "), strings.Join(e.Labels, " ")}, " "))
			var hit []string
			for _, w := range words {
				if len(w) > 3 && strings.Contains(hay, w) {
					hit = append(hit, w)
				}
			}
			if len(hit) > 0 {
				note(id, len(hit), "matches "+strings.Join(hit, ", "))
			}
		}
	}

	ids := make([]string, 0, len(score))
	for id := range score {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int { return cmp.Or(cmp.Compare(score[b], score[a]), cmp.Compare(a, b)) })
	limit := clamp(t.Limit, 10, 50)
	issueIDs := map[string]bool{}
	kept := 0
	for _, id := range ids {
		a := Applicable{Entry: l.entry(id, nil), Why: why[id], Score: score[id]}
		if l.binding(id) {
			if kept >= limit {
				res.Omitted++
				continue
			}
			res.Binding = append(res.Binding, a)
		} else {
			if kept >= limit {
				res.Omitted++
				continue
			}
			res.Informational = append(res.Informational, a)
		}
		kept++
		issueIDs[id] = true
	}
	res.Truncated = res.Omitted > 0
	if props, err := s.Proposals(ctx, repo); err == nil {
		words := strings.Fields(strings.ToLower(t.Query))
		for _, p := range props {
			if p.State != curation.Open || len(res.OpenProposals) >= 5 {
				continue
			}
			hay := strings.ToLower(p.Title + " " + p.Topic + " " + strings.Join(p.Components, " "))
			match := false
			for _, w := range words {
				match = match || (len(w) > 3 && strings.Contains(hay, w))
			}
			for _, c := range t.Components {
				match = match || slices.Contains(p.Components, strings.ToLower(strings.TrimSpace(c)))
			}
			if match {
				res.OpenProposals = append(res.OpenProposals, p)
			}
		}
	}
	for _, is := range l.issues() {
		if slices.ContainsFunc(is.Records, func(r string) bool { return issueIDs[r] }) {
			res.Issues = append(res.Issues, is)
		}
	}
	if len(res.Binding) == 0 {
		res.Notes = append(res.Notes, "no accepted decision or active standard names this code or matches the task; that is not the same as no constraint, so check the architecture overviews")
	}
	return res, nil
}

// maxFileSymbols bounds how many symbols of a file are checked for decisions.
const maxFileSymbols = 400

// decisionsInFile notes the records carried out by the symbols a file defines.
// One edge query covers them all.
func (s Service) decisionsInFile(ctx context.Context, repo string, file model.Node, byNode map[string]string, note func(id string, points int, reason string)) (map[string]string, error) {
	symbols, err := s.R.NodesInFile(ctx, repo, file.File)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(symbols)+len(byNode))
	name := map[string]string{}
	for _, sym := range symbols {
		if sym.Kind == model.KindSection || len(name) >= maxFileSymbols {
			continue
		}
		ids = append(ids, sym.ID)
		name[sym.ID] = sym.ShortName()
	}
	if len(ids) == 0 {
		return name, nil
	}
	for node := range byNode {
		ids = append(ids, node)
	}
	edges, err := s.R.Subgraph(ctx, ids, []model.EdgeKind{model.EdgeImplementsDecision})
	if err != nil {
		return name, err
	}
	for _, e := range edges {
		if id, ok := byNode[e.To]; ok && name[e.From] != "" {
			note(id, 4, fmt.Sprintf("implemented by %s in %s", name[e.From], file.File))
		}
	}
	return name, nil
}

// decisionsFor notes the records that code, its file and its package carry out.
func (s Service) decisionsFor(ctx context.Context, repo string, n model.Node, byNode map[string]string, note func(id string, points int, reason string)) {
	targets := []struct {
		id, label string
		points    int
	}{{n.ID, n.ShortName(), 5}}
	if n.Kind != model.KindFile && n.File != "" {
		targets = append(targets, struct {
			id, label string
			points    int
		}{model.NodeID(repo, model.KindFile, n.File), n.File, 3})
	}
	if n.Package != "" && n.Kind != model.KindPackage {
		targets = append(targets, struct {
			id, label string
			points    int
		}{model.NodeID(repo, model.KindPackage, n.Package), "package " + n.Package, 2})
	}
	for _, t := range targets {
		hits, err := s.R.Traverse(ctx, t.id, []model.EdgeKind{model.EdgeImplementsDecision}, store.Out, 1, maxLinks)
		if err != nil {
			continue // a node the index does not have (a package id derived from a file) just has no decisions
		}
		for _, h := range hits {
			if id, ok := byNode[h.Node.ID]; ok {
				note(id, t.points, "implemented by "+t.label)
			}
		}
	}
}
