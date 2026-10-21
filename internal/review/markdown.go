package review

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

func itoa(n int) string { return strconv.Itoa(n) }

// WriteMarkdown renders r as a compact Markdown context pack.
func WriteMarkdown(w io.Writer, r Report) error {
	var b strings.Builder
	s := r.Summary
	fmt.Fprintf(&b, "# Review context: %s\n\n", r.Repo)
	fmt.Fprintf(&b, "**Max risk: %s** · %d files · %d symbols changed · %d impacted · %d untested · %d security-sensitive\n\n",
		s.MaxLevel, s.ChangedFiles, s.ChangedSymbols, s.ImpactedSymbols, s.UntestedChanges, s.SecuritySensitive)

	if len(r.Changes) > 0 {
		b.WriteString("## Changed symbols (highest risk first)\n\n")
		b.WriteString("| Risk | Symbol | Location | Callers | Tests | Why |\n|---|---|---|---|---|---|\n")
		for _, c := range r.Changes {
			fmt.Fprintf(&b, "| %s %d | `%s` | %s | %d | %d | %s |\n",
				c.Level, c.Risk, c.Name, c.Location, c.FanIn, len(c.Tests), strings.Join(c.Reasons, "; "))
		}
		b.WriteString("\n")
	}

	if len(r.Impacted) > 0 {
		fmt.Fprintf(&b, "## Impacted (%d shown of %d)\n\n", len(r.Impacted), s.ImpactedSymbols)
		for _, i := range r.Impacted {
			fmt.Fprintf(&b, "- d%d `%s` %s\n", i.Depth, i.Name, i.Location)
		}
		b.WriteString("\n")
	}

	if len(r.Docs) > 0 {
		b.WriteString("## Docs that mention changed code\n\n")
		for _, d := range r.Docs {
			fmt.Fprintf(&b, "- %s (%s)\n", d.Name, d.Location)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Files\n\n")
	for _, f := range r.Files {
		note := ""
		if !f.Indexed && f.Status != "deleted" {
			note = " (not in graph)"
		}
		fmt.Fprintf(&b, "- %s `%s` · %d symbols%s\n", f.Status, f.Path, f.Symbols, note)
	}
	b.WriteString("\n")

	if len(r.Checklist) > 0 {
		b.WriteString("## Review checklist\n\n")
		for _, c := range r.Checklist {
			fmt.Fprintf(&b, "- [ ] %s\n", c)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}
