package security

import (
	"cmp"
	"slices"

	"github.com/astrona-io/codesynapse/internal/codeowners"
	"github.com/astrona-io/codesynapse/internal/model"
)

// OwnerCount is how many listed findings one CODEOWNERS owner has.
type OwnerCount struct {
	// Owner is an @user or @org/team handle, or "" for files no rule owns.
	Owner    string         `json:"owner"`
	Findings int            `json:"findings"`
	Counts   map[string]int `json:"counts"`
}

// applyOwners names the CODEOWNERS owners of every finding's file and
// counts the listed findings per owner, so each team sees its share. A
// finding with two owners counts for both. Nothing changes without a
// CODEOWNERS file.
func (rep *Report) applyOwners(rules []model.OwnerRule) {
	set := codeowners.Compile(rules)
	if set.Empty() {
		return
	}
	for _, list := range [][]Finding{rep.Findings, rep.Suppressed} {
		for i := range list {
			file, _, _ := splitLocation(list[i].Location)
			list[i].CodeOwners = set.Owners(file)
		}
	}
	by := map[string]*OwnerCount{}
	for _, f := range rep.Findings {
		owners := f.CodeOwners
		if len(owners) == 0 {
			owners = []string{""}
		}
		for _, o := range owners {
			c := by[o]
			if c == nil {
				c = &OwnerCount{Owner: o, Counts: map[string]int{}}
				by[o] = c
			}
			c.Findings++
			c.Counts[f.Severity]++
		}
	}
	rep.OwnerCounts = make([]OwnerCount, 0, len(by))
	for _, c := range by {
		rep.OwnerCounts = append(rep.OwnerCounts, *c)
	}
	// Most severe work first: by the count at each severity, highest first.
	slices.SortFunc(rep.OwnerCounts, func(a, b OwnerCount) int {
		for i := len(model.Severities) - 1; i >= 0; i-- {
			if c := cmp.Compare(b.Counts[model.Severities[i]], a.Counts[model.Severities[i]]); c != 0 {
				return c
			}
		}
		return cmp.Compare(a.Owner, b.Owner)
	})
}
