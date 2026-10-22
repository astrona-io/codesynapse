// Package duplicates finds code that exists more than once: inside one
// repository (Analyze, at ingest) and across the repositories a caller may
// see (Across, at request time from the stored reports).
//
// Detection uses Node.CloneHash, a structural fingerprint computed by the
// extractors, so no source is stored or compared here. A group is a set of
// functions with the same fingerprint; the redundant lines are what remains
// when one copy is kept.
package duplicates

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/parisnakitakejser/codesynapse/internal/model"
)

// Caps keep the stored report and the answers small.
const (
	MaxGroups       = 200
	MaxMembers      = 20
	MaxFingerprints = 2000
)

// Member is one copy.
type Member struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Package   string `json:"package,omitempty"`
	File      string `json:"file"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	LOC       int    `json:"loc"`
}

// Group is a set of copies of the same code inside one repository.
type Group struct {
	Hash   string `json:"hash"`
	Copies int    `json:"copies"`
	// Kind is "exact" (same structure) or "near" (edited copies: several
	// different fingerprints whose signatures are close). A near group lists
	// every copy of every variant.
	Kind string `json:"kind"`
	// Similarity is 1 for exact groups and, for near groups, the lowest
	// estimated similarity between the variants that were joined.
	Similarity float64 `json:"similarity"`
	// LOC is the size of the largest copy. Redundant is the lines left when
	// one copy is kept; for a near group only the differing variants count,
	// each once, because the copies inside a variant are in an exact group.
	LOC       int `json:"loc"`
	Redundant int `json:"redundant_loc"`
	// Scope is how far the copies are apart: file, package or repository.
	Scope   string   `json:"scope"`
	Members []Member `json:"members"`
}

// Fingerprint is one candidate function, kept so other repositories can be
// compared without reading this repository's graph.
type Fingerprint struct {
	Hash string `json:"hash"`
	// Sig is the MinHash signature used to find near copies.
	Sig string `json:"sig,omitempty"`
	Member
}

// Summary counts the report.
type Summary struct {
	Groups          int `json:"groups"`
	DuplicatedFuncs int `json:"duplicated_functions"`
	RedundantLOC    int `json:"redundant_loc"`
	// NearGroups and NearRedundantLOC count edited copies on top of the exact
	// ones; Percent covers both.
	NearGroups       int `json:"near_groups"`
	NearRedundantLOC int `json:"near_redundant_loc"`
	// FunctionLOC is the size of all non-test functions, the denominator of
	// Percent.
	FunctionLOC int     `json:"function_loc"`
	Percent     float64 `json:"percent"`
}

// Report is the duplicate-code analysis of one repository.
type Report struct {
	Summary Summary `json:"summary"`
	Groups  []Group `json:"groups"`
	// Fingerprints holds the largest candidates, including those without a
	// copy here.
	Fingerprints []Fingerprint `json:"fingerprints,omitempty"`
	// Truncated is set when a cap cut groups or fingerprints.
	Truncated bool `json:"truncated,omitempty"`
}

func member(n model.Node) Member {
	return Member{ID: n.ID, Name: n.QualifiedName, Package: n.Package, File: n.File, StartLine: n.StartLine, EndLine: n.EndLine, LOC: n.LOC}
}

// Analyze groups the functions of g by CloneHash.
func Analyze(g *model.Graph) Report {
	var rep Report
	byHash := map[string][]model.Node{}
	for _, n := range g.Nodes() {
		if n.Kind != model.KindFunction && n.Kind != model.KindMethod || n.IsTest {
			continue
		}
		rep.Summary.FunctionLOC += n.LOC
		if n.CloneHash != "" {
			byHash[n.CloneHash] = append(byHash[n.CloneHash], n)
		}
	}
	var prints []Fingerprint
	var variants []variant
	var variantHash []string
	for _, hash := range slices.Sorted(maps.Keys(byHash)) {
		nodes := byHash[hash]
		slices.SortFunc(nodes, func(a, b model.Node) int {
			return cmp.Or(cmp.Compare(a.File, b.File), cmp.Compare(a.StartLine, b.StartLine))
		})
		total, largest := 0, 0
		for _, n := range nodes {
			prints = append(prints, Fingerprint{Hash: hash, Sig: n.CloneSig, Member: member(n)})
			total += n.LOC
			largest = max(largest, n.LOC)
		}
		variants = append(variants, variant{sig: nodes[0].CloneSig, loc: largest})
		variantHash = append(variantHash, hash)
		if len(nodes) < 2 {
			continue
		}
		gr := Group{Hash: hash, Kind: "exact", Similarity: 1, Copies: len(nodes), LOC: largest, Redundant: total - largest, Scope: scope(nodes)}
		for _, n := range nodes[:min(len(nodes), MaxMembers)] {
			gr.Members = append(gr.Members, member(n))
		}
		rep.Groups = append(rep.Groups, gr)
		rep.Summary.Groups++
		rep.Summary.DuplicatedFuncs += len(nodes)
		rep.Summary.RedundantLOC += gr.Redundant
	}
	for _, c := range nearClusters(variants) {
		var nodes []model.Node
		extra, largest := 0, 0
		for _, i := range c.members {
			nodes = append(nodes, byHash[variantHash[i]]...)
			extra += variants[i].loc
			largest = max(largest, variants[i].loc)
		}
		slices.SortFunc(nodes, func(a, b model.Node) int {
			return cmp.Or(cmp.Compare(a.File, b.File), cmp.Compare(a.StartLine, b.StartLine))
		})
		gr := Group{Hash: variantHash[c.members[0]], Kind: "near", Similarity: c.similarity, Copies: len(nodes), LOC: largest, Redundant: extra - largest, Scope: scope(nodes)}
		for _, n := range nodes[:min(len(nodes), MaxMembers)] {
			gr.Members = append(gr.Members, member(n))
		}
		rep.Groups = append(rep.Groups, gr)
		rep.Summary.NearGroups++
		rep.Summary.NearRedundantLOC += gr.Redundant
	}
	if rep.Summary.FunctionLOC > 0 {
		rep.Summary.Percent = float64(rep.Summary.RedundantLOC+rep.Summary.NearRedundantLOC) * 100 / float64(rep.Summary.FunctionLOC)
	}
	slices.SortFunc(rep.Groups, func(a, b Group) int {
		return cmp.Or(cmp.Compare(b.Redundant, a.Redundant), cmp.Compare(a.Hash, b.Hash), cmp.Compare(a.Kind, b.Kind))
	})
	if len(rep.Groups) > MaxGroups {
		rep.Groups, rep.Truncated = rep.Groups[:MaxGroups], true
	}
	slices.SortFunc(prints, func(a, b Fingerprint) int {
		return cmp.Or(cmp.Compare(b.LOC, a.LOC), cmp.Compare(a.Hash, b.Hash), cmp.Compare(a.ID, b.ID))
	})
	if len(prints) > MaxFingerprints {
		prints, rep.Truncated = prints[:MaxFingerprints], true
	}
	rep.Fingerprints = prints
	return rep
}

func scope(nodes []model.Node) string {
	file, pkg := true, true
	for _, n := range nodes[1:] {
		file = file && n.File == nodes[0].File
		pkg = pkg && n.Package == nodes[0].Package
	}
	switch {
	case file:
		return "file"
	case pkg:
		return "package"
	}
	return "repository"
}

// Encode returns the report as JSON for the Repository node.
func Encode(r Report) (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("encoding duplicates report: %w", err)
	}
	return string(b), nil
}

// Decode reads a stored report; an empty string is an empty report.
func Decode(s string) (Report, error) {
	var r Report
	if s == "" {
		return r, nil
	}
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return r, fmt.Errorf("decoding duplicates report: %w", err)
	}
	return r, nil
}

// Info says whether one function has copies, for symbol cards.
type Info struct {
	Kind       string   `json:"kind"`
	Similarity float64  `json:"similarity"`
	Scope      string   `json:"scope"`
	Copies     int      `json:"copies"`
	Others     []Member `json:"others"`
}

const maxOthers = 5

// Lookup finds the group that contains the function id: its exact copies
// when it has any, else the edited copies. It returns nil when there are
// none (or the group listing was capped before reaching it).
func (r Report) Lookup(id string) *Info {
	var near *Info
	for _, g := range r.Groups {
		if !slices.ContainsFunc(g.Members, func(m Member) bool { return m.ID == id }) {
			continue
		}
		info := &Info{Kind: g.Kind, Similarity: g.Similarity, Scope: g.Scope, Copies: g.Copies, Others: []Member{}}
		for _, m := range g.Members {
			if m.ID != id && len(info.Others) < maxOthers {
				info.Others = append(info.Others, m)
			}
		}
		if g.Kind == "exact" {
			return info
		}
		near = info
	}
	return near
}
