package api

import (
	"cmp"
	"net/http"
	"slices"

	"github.com/astrona-io/codesynapse/internal/hygiene"
	"github.com/astrona-io/codesynapse/internal/license"
	"github.com/astrona-io/codesynapse/internal/model"
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
	// Latest, Deprecated and Behind come from deps.dev when lookups are on.
	Latest     string `json:"latest,omitempty"`
	Deprecated string `json:"deprecated,omitempty"`
	Behind     int    `json:"behind,omitempty"`
	// Retracted is the rationale of a retracted Go module version, from
	// the module proxy when lookups are on.
	Retracted string `json:"retracted,omitempty"`
	// Scorecard is the OpenSSF Scorecard of the package's source project,
	// from deps.dev when lookups are on.
	Scorecard *Scorecard `json:"scorecard,omitempty"`
}

// Scorecard is an OpenSSF Scorecard overall score (0 to 10), the day it
// was produced and up to three of its lowest checks, lowest first.
type Scorecard struct {
	Score  float64          `json:"score"`
	Date   string           `json:"date"`
	Checks []ScorecardCheck `json:"checks,omitempty"`
}

// ScorecardCheck is one Scorecard check by name with its score (0 to 10).
type ScorecardCheck struct {
	Name  string `json:"name"`
	Score int    `json:"score"`
}

// scorecardOf is the Scorecard stored on a Module node, nil when it has
// none; check entries that do not parse are dropped.
func scorecardOf(n model.Node) *Scorecard {
	if n.ScorecardDate == "" {
		return nil
	}
	sc := &Scorecard{Score: n.Scorecard, Date: n.ScorecardDate}
	for _, c := range n.ScorecardChecks {
		if ch, ok := hygiene.ParseCheck(c); ok {
			sc.Checks = append(sc.Checks, ScorecardCheck{Name: ch.Name, Score: ch.Score})
		}
	}
	return sc
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
			Direct: !n.Indirect, Dev: n.Dev, Via: n.Via, Roots: n.Roots, Latest: n.Latest, Deprecated: n.Deprecated, Behind: n.Behind, Retracted: n.Retracted,
			Scorecard: scorecardOf(n)})
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
