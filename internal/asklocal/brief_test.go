package asklocal

import (
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

// TestWorthBriefing: prompts from real sessions (PLAN item 18, 2026-10-28)
// that are instructions to the assistant get no brief; code tasks still do.
func TestWorthBriefing(t *testing.T) {
	for _, p := range []string{
		"start more agents",
		"i have new realse installed, v0.0.21 now, so we can countine",
		"ok, go ahead with the next one",
		"",
	} {
		assert.False(t, WorthBriefing(p), p)
	}
	for _, p := range []string{
		"fix the Stop hook counting upstream commits",
		"why does handleLogin return 503",
		"Make Handler validate its input before it runs",
		"rename prebrief.go to brief.go",
		"the dashboard throws an error after sign-in",
		"add a test for the session cache",
		"look at internal/asklocal and tell me what it checks",
	} {
		assert.True(t, WorthBriefing(p), p)
	}
}

func TestNothingRelevant(t *testing.T) {
	for _, a := range []string{
		"Nothing relevant in the given files.",
		"Nothing relevant… The developer should clarify which agents they mean (cmd/x.go:3).",
		"**Unclear** Nothing relevant in the given files.",
		"Not in the given files.",
	} {
		assert.True(t, NothingRelevant(a), a)
	}
	assert.False(t, NothingRelevant("**Where** Handler at app/app.go:3. **Unclear** whether nothing relevant to retries exists."))
}
