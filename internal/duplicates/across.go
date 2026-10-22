package duplicates

import (
	"cmp"
	"fmt"
	"path"
	"slices"
	"strings"
)

// Thresholds for recommending shared code. Below them the groups are still
// listed; they are just not worth a shared module on their own.
const (
	suggestMinGroups    = 3
	suggestMinRedundant = 50
	frameworkMinAreas   = 3
	maxCandidates       = 10
)

// CrossMember is one copy in one repository.
type CrossMember struct {
	Repo string `json:"repo"`
	Member
}

// CrossGroup is code found in two or more repositories.
type CrossGroup struct {
	Hash      string        `json:"hash"`
	Repos     []string      `json:"repos"`
	Copies    int           `json:"copies"`
	LOC       int           `json:"loc"`
	Redundant int           `json:"redundant_loc"`
	Members   []CrossMember `json:"members"`
}

// Suggestion recommends moving code that several repositories copy into one
// shared module (kind "library") or, when it spans several areas, a shared
// framework (kind "framework").
type Suggestion struct {
	Kind         string   `json:"kind"`
	Repos        []string `json:"repos"`
	Groups       int      `json:"groups"`
	RedundantLOC int      `json:"redundant_loc"`
	// Name is a proposed module name taken from the most common package.
	Name       string   `json:"name"`
	Candidates []string `json:"candidates"`
	Rationale  string   `json:"rationale"`
}

// CrossReport is the comparison of several repositories.
type CrossReport struct {
	Repos        int          `json:"repos"`
	Groups       []CrossGroup `json:"groups"`
	Suggestions  []Suggestion `json:"suggestions"`
	RedundantLOC int          `json:"redundant_loc"`
	// Partial lists repositories whose fingerprints were cut by a cap, so
	// copies of their smaller functions may be missing.
	Partial []string `json:"partial,omitempty"`
}

// Across finds code shared by two or more of the given reports, keyed by
// repository name. The caller decides which repositories may be compared;
// nothing outside the map is read.
func Across(reports map[string]Report) CrossReport {
	out := CrossReport{Repos: len(reports), Groups: []CrossGroup{}, Suggestions: []Suggestion{}}
	byHash := map[string][]CrossMember{}
	for repo, rep := range reports {
		if rep.Truncated {
			out.Partial = append(out.Partial, repo)
		}
		for _, f := range rep.Fingerprints {
			byHash[f.Hash] = append(byHash[f.Hash], CrossMember{Repo: repo, Member: f.Member})
		}
	}
	slices.Sort(out.Partial)
	for hash, ms := range byHash {
		repos := distinctRepos(ms)
		if len(repos) < 2 {
			continue
		}
		slices.SortFunc(ms, func(a, b CrossMember) int {
			return cmp.Or(cmp.Compare(a.Repo, b.Repo), cmp.Compare(a.File, b.File), cmp.Compare(a.StartLine, b.StartLine))
		})
		total, largest := 0, 0
		for _, m := range ms {
			total += m.LOC
			largest = max(largest, m.LOC)
		}
		gr := CrossGroup{Hash: hash, Repos: repos, Copies: len(ms), LOC: largest, Redundant: total - largest}
		gr.Members = ms[:min(len(ms), MaxMembers)]
		out.Groups = append(out.Groups, gr)
		out.RedundantLOC += gr.Redundant
	}
	slices.SortFunc(out.Groups, func(a, b CrossGroup) int {
		return cmp.Or(cmp.Compare(b.Redundant, a.Redundant), cmp.Compare(a.Hash, b.Hash))
	})
	out.Suggestions = suggest(out.Groups)
	if len(out.Groups) > MaxGroups {
		out.Groups = out.Groups[:MaxGroups]
	}
	return out
}

func distinctRepos(ms []CrossMember) []string {
	var repos []string
	for _, m := range ms {
		if !slices.Contains(repos, m.Repo) {
			repos = append(repos, m.Repo)
		}
	}
	slices.Sort(repos)
	return repos
}

// suggest bundles groups that exist in the same set of repositories: those
// repositories keep copying the same things, which a shared module fixes.
func suggest(groups []CrossGroup) []Suggestion {
	bySet := map[string][]CrossGroup{}
	for _, g := range groups {
		k := strings.Join(g.Repos, "\x00")
		bySet[k] = append(bySet[k], g)
	}
	out := []Suggestion{}
	for _, gs := range bySet {
		redundant := 0
		areas := map[string]int{}
		for _, g := range gs {
			redundant += g.Redundant
			for _, m := range g.Members {
				areas[path.Base(m.Package)]++
			}
		}
		if len(gs) < suggestMinGroups && redundant < suggestMinRedundant {
			continue
		}
		s := Suggestion{Kind: "library", Repos: gs[0].Repos, Groups: len(gs), RedundantLOC: redundant, Name: topArea(areas)}
		if len(areas) >= frameworkMinAreas {
			s.Kind = "framework"
		}
		for _, g := range gs[:min(len(gs), maxCandidates)] {
			s.Candidates = append(s.Candidates, g.Members[0].Name)
		}
		s.Rationale = fmt.Sprintf("%d functions (%d redundant lines) are copied between %s. Move one maintained copy into a shared %s (for example %q) that each of them imports, instead of fixing every copy.",
			len(gs), redundant, strings.Join(s.Repos, ", "), s.Kind, s.Name)
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b Suggestion) int {
		return cmp.Or(cmp.Compare(b.RedundantLOC, a.RedundantLOC), cmp.Compare(strings.Join(a.Repos, ","), strings.Join(b.Repos, ",")))
	})
	return out
}

func topArea(areas map[string]int) string {
	best, n := "shared", 0
	for a, c := range areas {
		if a == "" || a == "." {
			continue
		}
		if c > n || c == n && a < best {
			best, n = a, c
		}
	}
	return best
}
