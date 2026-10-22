package duplicates

import (
	"fmt"
	"io"
	"strings"
)

// WriteMarkdown renders the report of one repository, at most limit groups
// (0 = all).
func WriteMarkdown(w io.Writer, repo string, r Report, limit int) error {
	var b strings.Builder
	s := r.Summary
	fmt.Fprintf(&b, "# Duplicate code: %s\n\n", repo)
	fmt.Fprintf(&b, "**%d groups, %d functions, %d redundant lines (%.1f%% of function LOC)**\n\n", s.Groups, s.DuplicatedFuncs, s.RedundantLOC, s.Percent)
	if len(r.Groups) == 0 {
		b.WriteString("No duplicated functions found.\n")
	}
	for i, g := range r.Groups {
		if limit > 0 && i >= limit {
			fmt.Fprintf(&b, "\n… %d more groups\n", len(r.Groups)-limit)
			break
		}
		fmt.Fprintf(&b, "## %d copies of %d lines (%s) · %d redundant\n\n", g.Copies, g.LOC, g.Scope, g.Redundant)
		for _, m := range g.Members {
			fmt.Fprintf(&b, "- `%s` %s:%d-%d\n", m.Name, m.File, m.StartLine, m.EndLine)
		}
		b.WriteString("\n")
	}
	if r.Truncated {
		b.WriteString("\n_Report truncated: only the largest groups and candidates are kept._\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// WriteCrossMarkdown renders a comparison of repositories.
func WriteCrossMarkdown(w io.Writer, r CrossReport, limit int) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Shared code across %d repositories\n\n", r.Repos)
	fmt.Fprintf(&b, "**%d groups, %d redundant lines**\n\n", len(r.Groups), r.RedundantLOC)
	if len(r.Partial) > 0 {
		fmt.Fprintf(&b, "_Partial: smaller functions of %s were not compared._\n\n", strings.Join(r.Partial, ", "))
	}
	if len(r.Suggestions) > 0 {
		b.WriteString("## Recommended shared code\n\n")
		for _, s := range r.Suggestions {
			fmt.Fprintf(&b, "- **%s `%s`** between %s: %d groups, %d redundant lines. %s\n", s.Kind, s.Name, strings.Join(s.Repos, ", "), s.Groups, s.RedundantLOC, s.Rationale)
		}
		b.WriteString("\n")
	}
	for i, g := range r.Groups {
		if limit > 0 && i >= limit {
			fmt.Fprintf(&b, "\n… %d more groups\n", len(r.Groups)-limit)
			break
		}
		fmt.Fprintf(&b, "## %d copies of %d lines in %s\n\n", g.Copies, g.LOC, strings.Join(g.Repos, ", "))
		for _, m := range g.Members {
			fmt.Fprintf(&b, "- %s `%s` %s:%d-%d\n", m.Repo, m.Name, m.File, m.StartLine, m.EndLine)
		}
		b.WriteString("\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}
