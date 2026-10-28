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
		"No relevant code for the request is present. The provided files do not settle what the report asks (internal/redact/redact.go:12).",
		"**Where** No relevant code for the request is present in these ranges.",
	} {
		assert.True(t, NothingRelevant(a), a)
	}
	for _, a := range []string{
		"**Where** Handler at app/app.go:3. **Unclear** whether nothing relevant to retries exists.",
		"**Where** Login at api/login.go:40 returns 503 when the store is down. **Unclear** The provided files do not settle how often it retries.",
	} {
		assert.False(t, NothingRelevant(a), a)
	}
}

// TestHarnessTurnsAreNotBriefed: Claude Code fires UserPromptSubmit for
// turns it starts itself, with the text in "prompt" and no field that tells
// them apart. A subagent's hand-back from a real session (PLAN item 18,
// 2026-10-28) passed the gate on length and identifiers alone.
func TestHarnessTurnsAreNotBriefed(t *testing.T) {
	handBack := "Another Claude session sent a message:\n<agent-message from=\"a3e3303db7523bc45\" name=\"Telemetry phase 6 web metrics\">[Subagent hand-back] The text below is the final report of a subagent.\n  Added web server metrics in internal/telemetry/metrics.go: RecordWebRequest and webRouteLabel, with tests in telemetry_test.go. Fixed the Report helper in gateway/local.go.\n</agent-message>"
	notification := "<task-notification>\n<task-id>bw53zr0gx</task-id>\n<status>completed</status>\n<summary>Background command \"Run the telemetry tests\" completed (exit code 0)</summary>\n</task-notification>"
	for _, p := range []string{
		handBack,
		notification,
		"  " + notification,
		"<system-reminder>\nFix the failing TestStopGuard in cmd/codesynapse/stop.go\n</system-reminder>",
		"[SYSTEM NOTIFICATION] The background agent finished: fix handleLogin returning 503",
	} {
		assert.True(t, HarnessTurn(p), p)
		assert.False(t, WorthBriefing(p), p)
	}
	for _, p := range []string{
		"why does handleLogin return 503",
		"explain how <task-notification> turns reach the hook in cmd/codesynapse/prebrief.go",
	} {
		assert.False(t, HarnessTurn(p), p)
		assert.True(t, WorthBriefing(p), p)
	}
}
