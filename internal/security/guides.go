package security

import (
	"slices"
	"strings"

	"github.com/astrona-io/codesynapse/rules"
)

// Guide is a rule with its explanation and fix examples (rules/*/*.yaml).
type Guide = rules.Rule

// Example is a vulnerable/fixed pair.
type Example = rules.Example

// Catalog returns every rule, most severe first.
func Catalog() []Guide { return rules.All() }

// GuideFor returns the guide for rule, with examples for language first.
func GuideFor(rule, language string) (Guide, bool) {
	g, ok := rules.Get(rule)
	if !ok {
		return Guide{}, false
	}
	g.Examples = slices.Clone(g.Examples)
	if language != "" {
		want := normalizeLang(language)
		slices.SortStableFunc(g.Examples, func(a, b Example) int {
			am, bm := normalizeLang(a.Language) == want, normalizeLang(b.Language) == want
			switch {
			case am && !bm:
				return -1
			case bm && !am:
				return 1
			}
			return 0
		})
	}
	return g, true
}

func normalizeLang(l string) string {
	switch strings.ToLower(l) {
	case "javascript", "js", "typescript", "ts", "vue":
		return "typescript"
	case "yml", "yaml", "kubernetes", "helm":
		return "yaml"
	}
	return strings.ToLower(l)
}
