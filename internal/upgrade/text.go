package upgrade

import (
	"fmt"
	"io"
	"strings"
)

// maxPlaces is how many places each symbol lists in the text form.
const maxPlaces = 5

// WriteText writes the report for a terminal or a pull request comment.
func WriteText(w io.Writer, r *Report) error {
	var b strings.Builder
	target := r.From
	if r.To != "" {
		target = r.From + " → " + r.To
	}
	fmt.Fprintf(&b, "%s %s: %s of %s in %s\n", r.Module, target, plural(r.UseCount, "use"), plural(r.Symbols, "symbol"), plural(r.Files, "file"))
	switch {
	case r.To == "" || r.To == r.From:
	case len(r.Missing) > 0:
		verb := map[bool]string{true: "is", false: "are"}[len(r.Missing) == 1]
		fmt.Fprintf(&b, "\nNot compared: %s@%s %s not in the module cache. `go mod download` puts a version there; nothing here downloads.\n",
			r.Module, strings.Join(r.Missing, " and @"), verb)
	case len(r.Breaking) == 0:
		fmt.Fprintf(&b, "\nNothing this repository uses changed. The module's API: %d changed, %d removed, %d added.\n", r.Changed, r.Removed, r.Added)
	default:
		fmt.Fprintf(&b, "\n%s this repository uses changed:\n", plural(len(r.Breaking), "declaration"))
		for _, c := range r.Breaking {
			fmt.Fprintf(&b, "  %-7s %s (%s)\n", c.Kind, c.Symbol, plural(c.Uses, "use"))
			fmt.Fprintf(&b, "          was %s\n", c.Old)
			if c.Kind == "changed" {
				fmt.Fprintf(&b, "          now %s\n", c.New)
			}
			fmt.Fprintf(&b, "          at  %s\n", places(r.Uses, c.Symbol))
		}
		fmt.Fprintf(&b, "The module's whole API: %d changed, %d removed, %d added.\n", r.Changed, r.Removed, r.Added)
	}
	if len(r.Uses) > 0 {
		b.WriteString("\nUses:\n")
		for i, u := range r.Uses {
			if i > 0 && u.Symbol == r.Uses[i-1].Symbol {
				continue
			}
			fmt.Fprintf(&b, "  %s  %s\n", u.Symbol, places(r.Uses, u.Symbol))
		}
		if len(r.Uses) < r.UseCount {
			fmt.Fprintf(&b, "  (the first %d of %d uses; --json has them all up to the limit)\n", len(r.Uses), r.UseCount)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// places lists where sym is used, the first maxPlaces of them.
func places(uses []Use, sym string) string {
	var ps []string
	n := 0
	for _, u := range uses {
		if u.Symbol != sym {
			continue
		}
		if n++; n <= maxPlaces {
			p := fmt.Sprintf("%s:%d", u.File, u.Line)
			if u.In != "" {
				p += " (" + u.In + ")"
			}
			ps = append(ps, p)
		}
	}
	if n > maxPlaces {
		ps = append(ps, fmt.Sprintf("and %d more", n-maxPlaces))
	}
	return strings.Join(ps, ", ")
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
