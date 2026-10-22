// Package refactorplan says how to merge one group of duplicate functions:
// what to call the shared function, where it can live, which callers and
// tests are involved, what to watch for and the steps in order. It reads the
// graph only, never the source, and it changes nothing: the plan names what
// to open and what to check.
package refactorplan

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode"

	"github.com/parisnakitakejser/codesynapse/internal/duplicates"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/query"
	"github.com/parisnakitakejser/codesynapse/internal/store"
)

// ErrNotFound is returned when no such duplicate group is stored.
var ErrNotFound = errors.New("no such duplicate group; copy its hash and kind from the Duplicates page or duplicate_code")

const (
	maxCallers = 20
	maxTests   = 10
)

// Copy is one member of the group with what depends on it.
type Copy struct {
	query.Ref
	Signature string      `json:"signature,omitempty"`
	Exported  bool        `json:"exported,omitempty"`
	Tags      []string    `json:"security_tags,omitempty"`
	Liveness  string      `json:"liveness,omitempty"`
	Callers   []query.Ref `json:"callers"`
	Tests     []query.Ref `json:"tests"`
}

// Plan is the suggestion for one group.
type Plan struct {
	Hash       string  `json:"hash"`
	Kind       string  `json:"kind"`
	Similarity float64 `json:"similarity"`
	Scope      string  `json:"scope"`
	// Name is the suggested name of the shared function; Home where it can live.
	Name     string   `json:"name"`
	Home     string   `json:"home"`
	Approach string   `json:"approach"`
	Copies   []Copy   `json:"copies"`
	Steps    []string `json:"steps"`
	Risks    []string `json:"risks"`
	// Truncated is set when the group lists fewer copies than it has.
	Truncated bool `json:"truncated,omitempty"`
}

// ForGroup builds the plan for the group of repo's duplicate report with the
// given hash and kind ("exact" or "near"; "" means exact). r should be fenced
// to the repository.
func ForGroup(ctx context.Context, r store.Reader, repo, hash, kind string) (Plan, error) {
	if kind == "" {
		kind = "exact"
	}
	rep, ok := query.DuplicateReport(ctx, r, repo)
	if !ok {
		return Plan{}, fmt.Errorf("no duplicate report stored; re-ingest the repository: %w", ErrNotFound)
	}
	i := slices.IndexFunc(rep.Groups, func(g duplicates.Group) bool { return g.Hash == hash && g.Kind == kind })
	if i < 0 {
		return Plan{}, ErrNotFound
	}
	g := rep.Groups[i]
	svc := query.Service{R: r}
	p := Plan{Hash: g.Hash, Kind: g.Kind, Similarity: g.Similarity, Scope: g.Scope, Copies: []Copy{}, Truncated: g.Copies > len(g.Members)}

	var nodes []model.Node
	for _, m := range g.Members {
		n, err := r.Node(ctx, m.ID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue // the group is from before a change the graph already has
			}
			return Plan{}, err
		}
		callers, err := r.Traverse(ctx, n.ID, []model.EdgeKind{model.EdgeCalls, model.EdgeDispatchesTo}, store.In, 1, store.MaxLimit)
		if err != nil {
			return Plan{}, err
		}
		c := Copy{Ref: query.RefOf(n), Signature: n.Signature, Exported: n.Exported, Tags: n.SecurityTags, Liveness: n.Liveness, Callers: []query.Ref{}}
		for _, h := range callers {
			if !h.Node.IsTest && len(c.Callers) < maxCallers {
				c.Callers = append(c.Callers, query.RefOf(h.Node))
			}
		}
		if c.Tests, err = svc.Tests(ctx, n.ID, 3, maxTests); err != nil {
			return Plan{}, err
		}
		if c.Tests == nil {
			c.Tests = []query.Ref{}
		}
		p.Copies = append(p.Copies, c)
		nodes = append(nodes, n)
	}
	if len(nodes) < 2 {
		return Plan{}, ErrNotFound
	}
	p.Name = suggestName(nodes)
	p.Home = suggestHome(nodes, g.Scope)
	p.Approach = approach(nodes, g)
	p.Risks = risks(nodes, p.Copies, g)
	p.Steps = steps(p, nodes)
	return p, nil
}

// bareName is the last segment of a qualified name: "pkg.Type.Method" and
// "(*pkg.Type).Method" give "Method", "pkg.newID" gives "newID".
func bareName(n model.Node) string {
	name := n.Name
	if i := strings.LastIndexAny(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// suggestName is the name every copy shares, or the most common one, keeping
// the visibility of the public copies: renaming an exported copy breaks its
// importers.
func suggestName(nodes []model.Node) string {
	counts := map[string]int{}
	exported := false
	for _, n := range nodes {
		counts[bareName(n)]++
		exported = exported || n.Exported
	}
	best, top := "", 0
	for name, c := range counts {
		if c > top || c == top && name < best {
			best, top = name, c
		}
	}
	if exported && best != "" && !unicode.IsUpper([]rune(best)[0]) {
		r := []rune(best)
		r[0] = unicode.ToUpper(r[0])
		return string(r)
	}
	return best
}

func distinctNames(nodes []model.Node) []string {
	var names []string
	for _, n := range nodes {
		if b := bareName(n); !slices.Contains(names, b) {
			names = append(names, b)
		}
	}
	slices.Sort(names)
	return names
}

func suggestHome(nodes []model.Node, scope string) string {
	switch scope {
	case "file":
		return "the same file, " + nodes[0].File
	case "package":
		return "the same package (" + shortPkg(nodes[0]) + "), next to the copies"
	}
	dirs := make([]string, len(nodes))
	for i, n := range nodes {
		dirs[i] = path.Dir(n.File)
	}
	common := commonDir(dirs)
	if common == "" || common == "." {
		return "a new package (or module) that all the copies' packages can import; they share no directory"
	}
	return fmt.Sprintf("a package under %s (the closest directory the copies share), or a new package there if none fits", common)
}

func shortPkg(n model.Node) string {
	if n.Package == "" {
		return path.Dir(n.File)
	}
	return n.Package
}

func commonDir(dirs []string) string {
	parts := strings.Split(dirs[0], "/")
	for _, d := range dirs[1:] {
		other := strings.Split(d, "/")
		i := 0
		for i < len(parts) && i < len(other) && parts[i] == other[i] {
			i++
		}
		parts = parts[:i]
	}
	return strings.Join(parts, "/")
}

func approach(nodes []model.Node, g duplicates.Group) string {
	methods := 0
	for _, n := range nodes {
		if n.Kind == model.KindMethod {
			methods++
		}
	}
	var a string
	if names := distinctNames(nodes); len(names) > 1 {
		a = "The copies have different names (" + strings.Join(names, ", ") + "): name the shared function for what they have in common. "
	}
	switch {
	case methods == 0:
		a += "Extract one function with the shared body and make every copy call it, or delete the copies and call it directly."
	case methods == len(nodes):
		a += "The copies are methods. Extract a function that takes what the methods use from their receivers as arguments, and make each method a one-line call to it (or call it directly and drop the method)."
	default:
		a += "The copies are a mix of functions and methods. Extract one function, and make the others call it."
	}
	if g.Kind == "near" {
		a = fmt.Sprintf("These are edited copies (~%.0f%% alike). First work out what differs; each difference becomes a parameter, or stays in a thin wrapper around the shared part. ", g.Similarity*100) + a
	}
	return a
}

func risks(nodes []model.Node, copies []Copy, g duplicates.Group) []string {
	out := []string{}
	var public, internal, dead, tagged, untested []string
	pkgs := map[string]bool{}
	sigs := map[string]bool{}
	for i, n := range nodes {
		name := n.ShortName()
		if n.Exported {
			if strings.HasPrefix(n.File, "internal/") || strings.Contains(n.File, "/internal/") {
				internal = append(internal, name)
			} else {
				public = append(public, name)
			}
		}
		if n.Liveness == "unreachable" {
			dead = append(dead, name)
		}
		if len(n.SecurityTags) > 0 {
			tagged = append(tagged, name+" ["+strings.Join(n.SecurityTags, ",")+"]")
		}
		if len(copies[i].Tests) == 0 {
			untested = append(untested, name)
		}
		pkgs[n.Package] = true
		sigs[normalizeSignature(n)] = true
	}
	if len(dead) > 0 {
		out = append(out, "Nothing calls "+strings.Join(dead, ", ")+": delete such a copy instead of merging it.")
	}
	if len(public) > 0 {
		out = append(out, "Exported copies ("+strings.Join(public, ", ")+") may be public API: keep them as thin wrappers for a release, or update every importer in the same change.")
	}
	if len(internal) > 0 {
		out = append(out, "Exported copies in internal/ ("+strings.Join(internal, ", ")+") can only be imported inside this module: update their importers in the same change.")
	}
	if len(pkgs) > 1 {
		out = append(out, "The copies are in different packages: the shared function's home must not import any package that will call it, directly or through others, or you create an import cycle.")
	}
	if len(sigs) > 1 {
		out = append(out, "The signatures differ beyond names: decide the common parameters first, or keep a small adapter per copy.")
	}
	if len(untested) > 0 {
		out = append(out, "No test reaches "+strings.Join(untested, ", ")+": write one before merging so the behaviour of every copy is pinned.")
	}
	if len(tagged) > 0 {
		out = append(out, "Sensitive code ("+strings.Join(tagged, "; ")+"): review the merged function as security-relevant.")
	}
	if g.Kind == "near" {
		out = append(out, "A difference between edited copies is sometimes the point (a fix applied to only one): confirm with whoever changed them before folding it away.")
	}
	return out
}

// normalizeSignature drops the receiver and the function name so copies are
// compared by their parameters and results.
func normalizeSignature(n model.Node) string {
	s := n.Signature
	if i := strings.Index(s, "("); i >= 0 && strings.HasPrefix(s, "func (") {
		// func (recv) Name(params) results: skip the receiver group.
		if j := strings.Index(s, ") "); j > i {
			s = "func " + s[j+2:]
		}
	}
	if j := strings.Index(s, "("); j >= 0 && strings.HasPrefix(s, "func ") {
		s = "func" + s[j:]
	}
	return s
}

func steps(p Plan, nodes []model.Node) []string {
	out := []string{}
	n := 1
	add := func(format string, args ...any) {
		out = append(out, fmt.Sprintf("%d. %s", n, fmt.Sprintf(format, args...)))
		n++
	}
	callers := 0
	for _, c := range p.Copies {
		callers += len(c.Callers)
	}
	locs := make([]string, 0, len(nodes))
	for _, c := range p.Copies {
		locs = append(locs, c.Location)
	}
	if p.Kind == "near" {
		add("Open the copies side by side (%s) and list every difference.", strings.Join(locs, ", "))
	} else {
		add("Open one copy to confirm it is the one to keep (%s); the others are identical in structure.", locs[0])
	}
	add("Make sure each copy is covered by a test; add the missing ones first so the behaviour is pinned before it moves.")
	add("Create %s in %s and move the shared body there.", p.Name, p.Home)
	add("Replace the body of each copy with a call to it (%d callers across the copies are unaffected until you delete a copy).", callers)
	add("Run the tests and the build. Then delete the copies you no longer need, updating their callers.")
	add("Sync the repository and check that the group is gone from the Duplicates page.")
	return out
}
