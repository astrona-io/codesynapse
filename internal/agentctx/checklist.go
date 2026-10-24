package agentctx

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
)

// Change checklist (docs/PLAN.md, agent workflow roadmap step 8). Before a
// rename, a signature change or a removal, every place the change must also
// reach, from the graph's edges: callers and tests, the interface a method
// satisfies with that interface's other implementations (mocks flagged) and
// its callers, functions used as values, package importers, docs that
// mention the symbol, templates that render it and the decisions it carries
// out. The agent works through the list instead of grepping, and the notes
// say what the graph cannot see.

// Changes a checklist is for.
const (
	ChangeRename    = "rename"
	ChangeSignature = "signature"
	ChangeRemove    = "remove"
)

// ChecklistItem is one place a change must also reach.
type ChecklistItem struct {
	Ref
	Why string `json:"why"`
}

// ChecklistGroup is one kind of place.
type ChecklistGroup struct {
	Name  string          `json:"name"`
	Total int             `json:"total"`
	Items []ChecklistItem `json:"items"`
}

// Checklist is everything a change to Symbol must also reach.
type Checklist struct {
	Budget *ResponseBudget  `json:"budget,omitempty"`
	Symbol Ref              `json:"symbol"`
	Change string           `json:"change"`
	Total  int              `json:"total"`
	Groups []ChecklistGroup `json:"groups"`
	Notes  []string         `json:"notes"`
}

// checklistNotes are the blind spots of every checklist.
var checklistNotes = []string{
	"Calls through reflection, string names (RPC, templates by name, JSON tags) and other repositories are not in the graph; search for the name before you finish.",
}

// Checklist lists what a change to symbol must also reach. change is
// rename (default), signature or remove; limit bounds each group.
func (s Service) Checklist(ctx context.Context, repo, symbol, change string, limit, maxTokens int) (Checklist, error) {
	switch change {
	case "":
		change = ChangeRename
	case ChangeRename, ChangeSignature, ChangeRemove:
	default:
		return Checklist{}, fmt.Errorf("change must be rename, signature or remove, not %q", change)
	}
	s.R = store.ForRepository(s.R, repo)
	n, err := s.Resolve(ctx, repo, symbol)
	if err != nil {
		return Checklist{}, err
	}
	limit = store.ClampLimit(limit, 50)
	out := Checklist{Symbol: refOf(n, 0, false), Change: change, Groups: []ChecklistGroup{}, Notes: slices.Clone(checklistNotes)}
	out.Symbol.Summary = n.Signature
	seen := map[string]bool{n.ID: true}
	add := func(name string, hits []model.Hit, why func(model.Node) string, evidence map[string]model.Evidence) {
		g := ChecklistGroup{Name: name, Items: []ChecklistItem{}}
		for _, h := range hits {
			if seen[h.Node.ID] {
				continue
			}
			seen[h.Node.ID] = true
			g.Total++
			if len(g.Items) == limit {
				continue
			}
			item := ChecklistItem{Ref: refOf(h.Node, 0, false), Why: why(h.Node)}
			if e, ok := evidence[h.Node.ID]; ok {
				item.Evidence = e.OrUnknown()
			}
			g.Items = append(g.Items, item)
		}
		if g.Total > 0 {
			out.Groups = append(out.Groups, g)
			out.Total += g.Total
		}
	}
	verb := map[string]string{ChangeRename: "update the name", ChangeSignature: "update the arguments and results", ChangeRemove: "remove or replace it"}[change]

	// A method behind an interface: the interface method and every other
	// implementation change with it, and the interface's callers too.
	targets := []model.Node{n}
	if n.Kind == model.KindMethod && change != ChangeRemove {
		ifaces, err := s.traverse(ctx, n.ID, model.EdgeDispatchesTo, store.In)
		if err != nil {
			return Checklist{}, err
		}
		add("interface", ifaces, func(model.Node) string {
			return "the interface method this satisfies: " + verb + " here, then in every implementation"
		}, nil)
		var impls []model.Hit
		for _, h := range ifaces {
			targets = append(targets, h.Node)
			others, err := s.traverse(ctx, h.Node.ID, model.EdgeDispatchesTo, store.Out)
			if err != nil {
				return Checklist{}, err
			}
			impls = append(impls, others...)
		}
		add("implementations", impls, implementationWhy(verb), nil)
	}
	switch n.Kind {
	case model.KindInterfaceMethod:
		impls, err := s.traverse(ctx, n.ID, model.EdgeDispatchesTo, store.Out)
		if err != nil {
			return Checklist{}, err
		}
		add("implementations", impls, implementationWhy(verb), nil)
	case model.KindType:
		if n.Signature == "interface" {
			impls, err := s.traverse(ctx, n.ID, model.EdgeImplements, store.In)
			if err != nil {
				return Checklist{}, err
			}
			add("implementations", impls, implementationWhy(verb), nil)
		}
	case model.KindPackage:
		importers, err := s.traverse(ctx, n.ID, model.EdgeImports, store.In)
		if err != nil {
			return Checklist{}, err
		}
		add("importers", importers, func(model.Node) string { return "imports the package: update the import path" }, nil)
	}

	// Callers of the symbol and of the interface methods it satisfies;
	// tests apart, since they are rerun as well as edited.
	var callers, tests []model.Hit
	evidence := map[string]model.Evidence{}
	for _, t := range targets {
		hits, err := s.traverse(ctx, t.ID, model.EdgeCalls, store.In)
		if err != nil {
			return Checklist{}, err
		}
		if err := s.edgeEvidence(ctx, t.ID, hits, evidence); err != nil {
			return Checklist{}, err
		}
		for _, h := range hits {
			if h.Node.IsTest {
				tests = append(tests, h)
			} else {
				callers = append(callers, h)
			}
		}
	}
	add("callers", callers, func(c model.Node) string {
		if c.Kind == model.KindFile {
			return "package-level use in this file: " + verb
		}
		return "calls it: " + verb + " at the call"
	}, evidence)
	add("tests", tests, func(model.Node) string { return "a test calls it: " + verb + ", then run it" }, evidence)
	refs, err := s.traverse(ctx, n.ID, model.EdgeReferences, store.In)
	if err != nil {
		return Checklist{}, err
	}
	add("references", refs, func(model.Node) string {
		return "uses it as a value (handler, callback, method value): " + verb + " where it is passed"
	}, nil)
	switch n.Kind {
	case model.KindTemplateDef:
		incs, err := s.traverse(ctx, n.ID, model.EdgeIncludes, store.In)
		if err != nil {
			return Checklist{}, err
		}
		add("templates", incs, func(model.Node) string { return "includes this define: " + verb + " in the include" }, nil)
	case model.KindConfigEntry:
		uses, err := s.traverse(ctx, n.ID, model.EdgeUsesValue, store.In)
		if err != nil {
			return Checklist{}, err
		}
		add("templates", uses, func(model.Node) string { return "reads this value (.Values…): " + verb + " in the template" }, nil)
	}
	docs, err := s.traverse(ctx, n.ID, model.EdgeMentions, store.In)
	if err != nil {
		return Checklist{}, err
	}
	add("docs", docs, func(model.Node) string { return "mentions it: update the text and any examples" }, nil)
	records, err := s.traverse(ctx, n.ID, model.EdgeImplementsDecision, store.Out)
	if err != nil {
		return Checklist{}, err
	}
	add("decisions", records, func(model.Node) string {
		return "this code carries out the decision: check the change still follows it"
	}, nil)

	if n.Kind == model.KindType {
		out.Notes = append(out.Notes, "Uses of a type in declarations, fields and conversions are not edges in the graph; let the compiler find them after the change.")
	}
	if change == ChangeRemove && len(tests) > 0 {
		out.Notes = append(out.Notes, "The tests listed exercise what is being removed: delete or rewrite them.")
	}
	return finishChecklist(out, maxTokens)
}

func implementationWhy(verb string) func(model.Node) string {
	return func(m model.Node) string {
		if isMock(m) {
			return "a mock or fake implements it: " + verb + " here too"
		}
		return "implements it: " + verb + " the same way"
	}
}

// isMock recognises test doubles: implementations in test files, or named
// as one.
func isMock(n model.Node) bool {
	if n.IsTest || strings.HasSuffix(n.File, "_test.go") {
		return true
	}
	name := strings.ToLower(n.QualifiedName)
	return strings.Contains(name, "mock") || strings.Contains(name, "fake") || strings.Contains(name, "stub")
}

func (s Service) traverse(ctx context.Context, id string, kind model.EdgeKind, dir store.Direction) ([]model.Hit, error) {
	return s.R.Traverse(ctx, id, []model.EdgeKind{kind}, dir, 1, store.MaxLimit)
}

// edgeEvidence records how each hit's edge to root was found.
func (s Service) edgeEvidence(ctx context.Context, root string, hits []model.Hit, into map[string]model.Evidence) error {
	if len(hits) == 0 {
		return nil
	}
	ids := []string{root}
	for _, h := range hits {
		ids = append(ids, h.Node.ID)
	}
	edges, err := s.R.Subgraph(ctx, ids, callEdges)
	if err != nil {
		return err
	}
	for _, e := range edges {
		if e.To == root {
			if cur, ok := into[e.From]; !ok || e.Evidence.Stronger(cur) {
				into[e.From] = e.Evidence
			}
		}
	}
	return nil
}

func finishChecklist(out Checklist, maxTokens int) (Checklist, error) {
	err := fitResponse(&out, &out.Budget, maxTokens, func() int {
		// Shed items from the largest group first; totals stay.
		largest := -1
		for i, g := range out.Groups {
			if len(g.Items) > 0 && (largest < 0 || len(g.Items) > len(out.Groups[largest].Items)) {
				largest = i
			}
		}
		if largest < 0 {
			return 0
		}
		return trimTail(&out.Groups[largest].Items)
	})
	return out, err
}
