package cleanup

import (
	"fmt"
	"io"
	"strings"
)

// Steps is the safe-removal checklist shown with every report.
var Steps = []string{
	"Start with security-relevant items: dead code that calls exec, SQL, crypto or a vulnerable symbol is pure risk.",
	"Remove high-confidence items first (unexported functions); the compiler confirms the rest of the build still links.",
	"For exported or low-confidence items, search for reflection, plugins, go:linkname and external consumers before deleting.",
	"Delete unused go.mod requirements with `go mod tidy` and re-run `codesynapse audit` to confirm the CVEs are gone.",
	"Test-only code: delete it with the tests that use it, or move it into _test.go files so it is not shipped.",
	"Run the tests, re-ingest (Sync), and check that the dead-code count went down and nothing became unreachable unexpectedly.",
}

// WriteMarkdown renders r.
func WriteMarkdown(w io.Writer, repo string, r Report, limit int) error {
	var b strings.Builder
	s := r.Summary
	fmt.Fprintf(&b, "# Unused code: %s\n\n", repo)
	fmt.Fprintf(&b, "**%d of %d functions unused (%.1f%% of function LOC, %d lines)** · %d unreachable · %d test-only · %d security-relevant · %d dead files · %d unused modules (%d CVEs removable) · %d unused Helm values · %d unused template defines\n\n",
		s.Unreachable+s.TestOnly, s.Functions, s.DeadPct, s.DeadLOC, s.Unreachable, s.TestOnly, s.SecurityRelevant,
		s.DeadFiles, s.UnusedModules, s.CVEsRemovable, s.UnusedValues, s.UnusedDefines)
	if len(r.Modules) > 0 {
		b.WriteString("## Unused dependencies\n\n")
		for _, m := range r.Modules {
			vulns := ""
			if len(m.Vulns) > 0 {
				vulns = " · removes " + strings.Join(m.Vulns, ", ")
			}
			fmt.Fprintf(&b, "- `%s@%s`%s\n", m.Path, m.Version, vulns)
		}
		b.WriteString("\n")
	}
	if len(r.Items) > 0 {
		b.WriteString("## Functions (security-relevant and high confidence first)\n\n| Category | Confidence | Symbol | Location | LOC | Security |\n|---|---|---|---|---|---|\n")
		for i, it := range r.Items {
			if limit > 0 && i >= limit {
				fmt.Fprintf(&b, "\n…and %d more.\n", len(r.Items)-limit)
				break
			}
			fmt.Fprintf(&b, "| %s | %s | `%s` | %s | %d | %s |\n", it.Category, it.Confidence, it.Name, it.Location, it.LOC, strings.Join(it.Security, "; "))
		}
		b.WriteString("\n")
	}
	if len(r.TestSupport) > 0 {
		fmt.Fprintf(&b, "Test-support packages (only tests use them, not counted): %s\n\n", strings.Join(r.TestSupport, ", "))
	}
	if len(r.Files) > 0 {
		b.WriteString("## Files with nothing live\n\n")
		for _, f := range r.Files {
			fmt.Fprintf(&b, "- `%s` · %d symbols, %d lines\n", f.File, f.Symbols, f.LOC)
		}
		b.WriteString("\n")
	}
	b.WriteString("## How to clean up safely\n\n")
	for i, step := range Steps {
		fmt.Fprintf(&b, "%d. %s\n", i+1, step)
	}
	_, err := io.WriteString(w, b.String())
	return err
}
