package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/asklocal"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// caseGPrompt is the prompt of PLAN item 18, case g (2026-10-29).
const caseGPrompt = `to cut down the information the explaining below the stats pls make it a 'info' icon with a mouse over eg. "CodeSynapse's own AI tasks: prose a local model shortened before it went to the cloud, at the input price; gateway traffic is passed on unchanged" are only interesting if you hold mouse over`

// caseGPlan is what the graph's plan chose for it: Go Stats structs and
// builders that only share the word "stats".
var caseGPlan = []string{
	"internal/model/model.go:456-475",
	"internal/memory/memory.go:367-379",
	"internal/semantic/store.go:41-45",
	"services/api/internal/api/stats.go:37-49",
	"internal/store/stats.go:67-74",
	"internal/store/memory/memory.go:248-265",
	"internal/store/nebulastore/list.go:236-288",
	"internal/app/cli.go:125-164",
}

// quoteCheckout is a checkout with the page that holds the quoted hint (as
// the code writes it: an escaped apostrophe inside a bound attribute) and
// the Go files of caseGPlan.
func quoteCheckout(t *testing.T) string {
	t.Helper()
	dir := gitCheckout(t)
	write := func(p, s string) {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, p), []byte(s), 0o600))
	}
	page := strings.Repeat("<!-- filler -->\n", 137) +
		`          :hint="!compressionOn ? compressionOffHint : 'CodeSynapse\'s own AI tasks: prose a local model shortened before it went to the cloud, at the input price; gateway traffic is passed on unchanged'"` + "\n" +
		strings.Repeat("<!-- filler -->\n", 60)
	write("services/web/app/pages/stats/models.vue", page)
	for _, r := range caseGPlan {
		p, _, _, err := asklocal.ParseSpec(r)
		require.NoError(t, err)
		write(p, "package x\n\n"+strings.Repeat("// Stats counts things.\ntype Stats struct{ N int }\n", 150))
	}
	// Not tracked: never searched.
	write("notes.txt", "CodeSynapse's own AI tasks: prose a local model shortened before it went to the cloud\n")
	_, err := gitx.Run(context.Background(), dir, "add", "services", "internal")
	require.NoError(t, err)
	return dir
}

// TestBriefRangesFollowQuotedText: PLAN item 18, case g. The prompt quoted a
// hint of the Models statistics page; the plan matched "stats" to Go Stats
// structs and the brief said the phrase was in none of them. The quoted text
// now picks the page, and a quote that is nowhere picks nothing in its place.
func TestBriefRangesFollowQuotedText(t *testing.T) {
	dir := quoteCheckout(t)
	cases := []struct {
		name       string
		prompt     string
		plan       []string
		want       []string
		wantMissed bool
	}{{
		name:   "case g: the quote selects the page, not the Go Stats structs",
		prompt: caseGPrompt,
		plan:   caseGPlan,
		want:   []string{"services/web/app/pages/stats/models.vue:118-158"},
	}, {
		name:   "curly quotes and a graph range in the same file",
		prompt: "make “gateway traffic is passed on unchanged” an info icon on hover",
		plan:   append([]string{"services/web/app/pages/stats/models.vue:1-20"}, caseGPlan...),
		want:   []string{"services/web/app/pages/stats/models.vue:118-158", "services/web/app/pages/stats/models.vue:1-20"},
	}, {
		name:       "a quote in no tracked file: no invented matches, fewer ranges",
		prompt:     `turn "the quarterly revenue forecast widget" into an info icon with a mouse over`,
		plan:       caseGPlan,
		want:       caseGPlan[:maxRangesAfterMiss],
		wantMissed: true,
	}, {
		name:   "no quote: the graph's plan unchanged",
		prompt: "make the Stats struct count tokens too",
		plan:   caseGPlan,
		want:   caseGPlan,
	}, {
		name:   "a quote too short to be text: the graph's plan unchanged",
		prompt: `rename "Stats" to "Totals" in the store`,
		plan:   caseGPlan,
		want:   caseGPlan,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, missed := briefRanges(context.Background(), dir, tc.prompt, tc.plan)
			assert.Equal(t, missed, tc.wantMissed)
			require.Len(t, got, len(tc.want))
			for i := range tc.want {
				assert.Equal(t, got[i], tc.want[i])
			}
		})
	}
}

// The whole pre-brief for a quote that is in no file: the agent is told so,
// even when the model finds nothing in the few ranges left.
func TestPrebriefSaysWhenAQuoteIsNowhere(t *testing.T) {
	dir := askCheckout(t)
	in := hookInput{SessionID: "chat-1", Cwd: dir, Prompt: `make One show "the quarterly revenue forecast widget" as a tooltip`}
	var asked asklocal.Request
	api := prebriefAPI(t, filepath.Base(dir), asklocal.Answer{Answer: "Nothing relevant in the given files.", Grounded: true}, &asked)
	text, err := prebrief(context.Background(), apiClient{base: api.URL, token: "csk_test", http: api.Client()}, in)
	require.NoError(t, err)
	assert.Equal(t, text, quoteMissedNote)

	grounded := asklocal.Answer{Answer: "**Where** One at pkg/one.go:3.", Model: "qwen3.8:27b", Grounded: true, Citations: []asklocal.Citation{{Path: "pkg/one.go", Start: 3, End: 3}}}
	api = prebriefAPI(t, filepath.Base(dir), grounded, &asked)
	text, err = prebrief(context.Background(), apiClient{base: api.URL, token: "csk_test", http: api.Client()}, in)
	require.NoError(t, err)
	assert.Contains(t, text, quoteMissedNote+"\n\n**Where** One at pkg/one.go:3.")
}
