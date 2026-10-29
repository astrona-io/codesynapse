package asklocal

import (
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

const caseGQuote = "CodeSynapse's own AI tasks: prose a local model shortened before it went to the cloud, at the input price; gateway traffic is passed on unchanged"

func TestQuotedPhrases(t *testing.T) {
	cases := []struct {
		name, prompt string
		want         []string
	}{
		{"straight quotes, PLAN item 18 case g", `make it a 'info' icon with a mouse over eg. "` + caseGQuote + `" are only interesting on hover`, []string{caseGQuote}},
		{"curly quotes", "make “gateway traffic is passed on unchanged” an icon", []string{"gateway traffic is passed on unchanged"}},
		{"guillemets", "the «Saved by shorter prompts» line", []string{"Saved by shorter prompts"}},
		{"too short to be text", `rename "Stats" to "Totals"`, nil},
		{"apostrophes are not quotes", "it's the user's page and don't touch it's handler", nil},
		{"no quote", "make the Stats struct count tokens", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, QuotedPhrases(tc.prompt), tc.want)
		})
	}
	assert.Len(t, QuotedPhrases(`"a b c" "d e f" "g h i" "j k l"`), MaxQuotes)
}

func TestQuoteFragments(t *testing.T) {
	assert.Equal(t, QuoteFragments(caseGQuote),
		[]string{"own ai tasks", "prose a local model shortened before it went to the cloud", "at the input price", "gateway traffic is passed on unchanged"})
	assert.Equal(t, QuoteFragments("Saved by shorter prompts"), []string{"saved by shorter prompts"})
	assert.Len(t, QuoteFragments("a, b, c"), 0)
}

func TestAboutUIAndFrontEnd(t *testing.T) {
	for _, p := range []string{"make it an info icon with a mouse over", "show it in a tooltip", "the button on the stats page", "hover shows the hint"} {
		assert.True(t, AboutUI(p), p)
	}
	for _, p := range []string{"make the Stats struct count tokens", "why does handleLogin return 503"} {
		assert.False(t, AboutUI(p), p)
	}
	for _, p := range []string{"services/web/app/pages/stats/models.vue", "services/web/app/composables/useApi.ts", "src/App.tsx", "app/assets/css/main.css"} {
		assert.True(t, FrontEnd(p), p)
	}
	for _, p := range []string{"internal/store/stats.go", "services/api/internal/api/stats.go", "cmd/tool/main.ts", "docs/AGENT_GUIDE.md"} {
		assert.False(t, FrontEnd(p), p)
	}
}
