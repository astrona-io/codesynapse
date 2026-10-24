package security

import (
	"fmt"
	"slices"
)

// OnlyFiles narrows the report to findings in files (repository-relative),
// for a check of a change such as a pre-commit hook: the grade still
// describes the whole repository, so the report says what it covers in
// Scope instead. Rule and severity counts are recounted over what is
// left; owner counts are dropped. Vulnerabilities stay, since a lockfile
// is not where an advisory lives.
func (r Report) OnlyFiles(files []string, scope string) Report {
	in := func(f Finding) bool {
		file, _, _ := splitLocation(f.Location)
		return slices.Contains(files, file)
	}
	r.Scope = scope
	r.Findings = slices.DeleteFunc(slices.Clone(r.Findings), func(f Finding) bool { return !in(f) })
	r.Suppressed = slices.DeleteFunc(slices.Clone(r.Suppressed), func(f Finding) bool { return !in(f) })
	r.FindingCounts, r.SuppressedCounts = map[string]int{}, map[string]int{}
	r.OwnerCounts = nil
	rules := map[string]int{}
	for _, f := range r.Findings {
		r.FindingCounts[f.Severity]++
		rules[f.Rule]++
	}
	for _, f := range r.Suppressed {
		r.SuppressedCounts[f.Severity]++
	}
	kept := r.Rules[:0:0]
	for _, rc := range r.Rules {
		if n := rules[rc.Rule]; n > 0 {
			rc.Count = n
			kept = append(kept, rc)
		}
	}
	r.Rules = kept
	if r.Unanalysed > 0 {
		// The listed findings stop at the store's limit; the rest are only
		// counted, and may be in these files.
		r.Scope += fmt.Sprintf("; %d findings of the repository are beyond the listed ones and not checked here", r.Unanalysed)
		r.Unanalysed = 0
	}
	return r
}
