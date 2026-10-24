package api

import (
	"cmp"
	"net/http"
	"slices"

	"github.com/astrona-io/codesynapse/internal/license"
	"github.com/astrona-io/codesynapse/internal/store"
)

// Package is one dependency in the dashboard's list.
type Package struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
	Version   string `json:"version,omitempty"`
	// License is the SPDX expression, "unknown" when none was found, the
	// same names as the licence report's by_license counts.
	License string `json:"license"`
	Direct  bool   `json:"direct"`
	Dev     bool   `json:"dev,omitempty"`
	Via     string `json:"via,omitempty"`
	Roots   int    `json:"roots,omitempty"`
}

// EcosystemCount is how many packages one ecosystem contributes.
type EcosystemCount struct {
	Ecosystem string `json:"ecosystem"`
	Count     int    `json:"count"`
}

// Dependencies is every package of a repository, complete up to
// store.MaxDependencies (unlike the generic list, which stops at 500).
type Dependencies struct {
	Packages    []Package        `json:"packages"`
	ByEcosystem []EcosystemCount `json:"by_ecosystem"`
	Truncated   bool             `json:"truncated,omitempty"`
}

// handleDependencies lists the repository's packages for the Dependencies
// and Licences pages, which group them by ecosystem and by licence.
func (s *Server) handleDependencies(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	g, err := store.ForRepository(s.st, repo).DependencyGraph(r.Context(), repo)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	writeJSON(w, 200, dependenciesOf(g))
}

func dependenciesOf(g store.DependencyGraph) Dependencies {
	out := Dependencies{Packages: []Package{}, ByEcosystem: []EcosystemCount{}, Truncated: g.Truncated}
	counts := map[string]int{}
	for _, n := range g.Modules {
		eco := cmp.Or(n.Ecosystem, license.Go)
		if eco == license.Go && n.Name == "stdlib" {
			continue // ships with every Go program; the licence report leaves it out too
		}
		out.Packages = append(out.Packages, Package{Ecosystem: eco, Name: n.Name, Version: n.Version, License: cmp.Or(n.License, "unknown"),
			Direct: !n.Indirect, Dev: n.Dev, Via: n.Via, Roots: n.Roots})
		counts[eco]++
	}
	slices.SortFunc(out.Packages, func(a, b Package) int {
		return cmp.Or(cmp.Compare(a.Ecosystem, b.Ecosystem), cmp.Compare(a.Name, b.Name), cmp.Compare(a.Version, b.Version))
	})
	for eco, n := range counts {
		out.ByEcosystem = append(out.ByEcosystem, EcosystemCount{Ecosystem: eco, Count: n})
	}
	slices.SortFunc(out.ByEcosystem, func(a, b EcosystemCount) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Ecosystem, b.Ecosystem))
	})
	return out
}
