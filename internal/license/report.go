package license

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// maxIssues bounds the dependencies listed individually in a report; the
// counts always cover every dependency.
const maxIssues = 1000

// Dependency is the input to Build: one resolved dependency.
type Dependency struct {
	Ecosystem string
	Name      string
	Version   string
	License   string
	Source    string
	Indirect  bool
	Dev       bool
}

// Entry is a dependency with its verdict.
type Entry struct {
	Ecosystem string   `json:"ecosystem"`
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	License   string   `json:"license,omitempty"`
	Source    string   `json:"source,omitempty"`
	Category  Category `json:"category"`
	Direct    bool     `json:"direct"`
	Dev       bool     `json:"dev,omitempty"`
	Verdict   Verdict  `json:"verdict"`
}

// Count is the number of dependencies under one licence.
type Count struct {
	License  string   `json:"license"`
	Category Category `json:"category"`
	Count    int      `json:"count"`
}

// Report is the licence check of one repository, computed at ingest and
// stored on the Repository node.
type Report struct {
	Project  Found    `json:"project"`
	Category Category `json:"category"`
	Total    int      `json:"total"`
	OK       int      `json:"ok"`
	Review   int      `json:"review"`
	Conflict int      `json:"conflict"`
	// Unknown counts dependencies without a licence found (a subset of Review).
	Unknown   int     `json:"unknown"`
	ByLicense []Count `json:"by_license"`
	// Issues lists review and conflict verdicts, conflicts first.
	Issues []Entry `json:"issues"`
	// Truncated is the number of issues left out over maxIssues.
	Truncated int `json:"truncated,omitempty"`
	// Online says whether deps.dev was asked for licences missing on disk.
	Online bool `json:"online"`
}

var levelRank = map[string]int{Conflict: 0, Review: 1, OK: 2}

// Build checks every dependency against the project licence.
func Build(project Found, deps []Dependency, online bool) Report {
	rep := Report{Project: project, Category: Classify(project.License), Online: online, ByLicense: []Count{}, Issues: []Entry{}}
	if project.License == "" {
		rep.Category = Unknown
	}
	counts := map[string]*Count{}
	for _, d := range deps {
		if d.Ecosystem == Go && d.Name == "stdlib" {
			continue // ships with every Go program under BSD-3-Clause
		}
		e := Entry{Ecosystem: d.Ecosystem, Name: d.Name, Version: d.Version, License: d.License, Source: d.Source,
			Category: Classify(d.License), Direct: !d.Indirect, Dev: d.Dev, Verdict: Check(project.License, d.License, d.Dev)}
		rep.Total++
		switch e.Verdict.Level {
		case OK:
			rep.OK++
		case Review:
			rep.Review++
		case Conflict:
			rep.Conflict++
		}
		if d.License == "" {
			rep.Unknown++
		}
		name := cmp.Or(d.License, "unknown")
		if counts[name] == nil {
			counts[name] = &Count{License: name, Category: e.Category}
		}
		counts[name].Count++
		if e.Verdict.Level != OK {
			rep.Issues = append(rep.Issues, e)
		}
	}
	for _, c := range counts {
		rep.ByLicense = append(rep.ByLicense, *c)
	}
	slices.SortFunc(rep.ByLicense, func(a, b Count) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.License, b.License))
	})
	slices.SortFunc(rep.Issues, func(a, b Entry) int {
		return cmp.Or(cmp.Compare(levelRank[a.Verdict.Level], levelRank[b.Verdict.Level]),
			boolFirst(a.Direct && !a.Dev, b.Direct && !b.Dev), cmp.Compare(a.Ecosystem, b.Ecosystem), cmp.Compare(a.Name, b.Name))
	})
	if len(rep.Issues) > maxIssues {
		rep.Truncated = len(rep.Issues) - maxIssues
		rep.Issues = rep.Issues[:maxIssues]
	}
	return rep
}

func boolFirst(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return -1
	}
	return 1
}

// Encode serialises a report for the Repository node.
func Encode(r Report) (string, error) {
	b, err := json.Marshal(r)
	return string(b), err
}

// Markdown renders a report for the CLI and MCP.
func Markdown(repo string, r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Licences: %s\n\n", repo)
	if r.Project.License == "" {
		b.WriteString("**Project licence:** none found (no LICENSE file, package.json or pyproject.toml licence). Without one, nobody may reuse the code.\n\n")
	} else {
		fmt.Fprintf(&b, "**Project licence:** %s (%s), from %s\n\n", r.Project.License, r.Category, r.Project.Source)
	}
	fmt.Fprintf(&b, "%d dependencies: %d ok, %d to review, %d conflicts (%d without a known licence).", r.Total, r.OK, r.Review, r.Conflict, r.Unknown)
	if !r.Online {
		b.WriteString(" Licences were read from disk only; enable the deps.dev lookup to fill in the unknown ones.")
	}
	b.WriteString("\n\n")
	if len(r.Issues) > 0 {
		b.WriteString("| Verdict | Dependency | Licence | Read from | Why |\n|---|---|---|---|---|\n")
		for _, e := range r.Issues {
			scope := "indirect"
			if e.Direct {
				scope = "direct"
			}
			if e.Dev {
				scope += ", dev"
			}
			fmt.Fprintf(&b, "| %s | %s %s@%s (%s) | %s | %s | %s |\n", e.Verdict.Level, e.Ecosystem, e.Name, e.Version, scope,
				cmp.Or(e.License, "unknown"), cmp.Or(e.Source, "-"), strings.ReplaceAll(e.Verdict.Reason, "|", "\\|"))
		}
		if r.Truncated > 0 {
			fmt.Fprintf(&b, "\n%d more issues not listed.\n", r.Truncated)
		}
		b.WriteString("\n")
	}
	b.WriteString("Licences in use:\n\n")
	for _, c := range r.ByLicense {
		fmt.Fprintf(&b, "- %s (%s): %d\n", c.License, c.Category, c.Count)
	}
	b.WriteString("\nThis is guidance, not legal advice: check each verdict's source before relying on it.\n")
	return b.String()
}
