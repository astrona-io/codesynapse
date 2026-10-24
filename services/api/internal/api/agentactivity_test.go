package api

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/agentcfg"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestAgentActivityIsolationAndValidation(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	body := `{"client":"codex","event":"PostToolUse","category":"shell","outcome":"ok","duration_ms":42,"input_bytes":12}`
	path := "/api/v1/execution/activity"
	assert.Equal(t, f.client.do("POST", path, body, nil), 403)
	assert.Equal(t, f.client.do("POST", path, body, nil, runtimeHeaders("unknown")...), 403)
	require.Equal(t, f.client.do("POST", path, body, nil, runtimeHeaders("thread-a")...), 201)
	for _, bad := range []string{
		`{"client":"codex","event":"PostToolUse","category":"secret command","outcome":"ok"}`,
		`{"client":"codex","event":"PostToolUse","category":"shell","outcome":"ok","prompt":"private"}`,
		`{"client":"codex","event":"Invented","category":"shell","outcome":"ok"}`,
		`{"client":"codex","event":"PostToolUse","category":"shell","outcome":"ok","duration_ms":-1}`,
		`{"client":"codex","event":"PostToolUse","category":"shell","outcome":"ok"} {}`,
	} {
		assert.Equal(t, f.client.do("POST", path, bad, nil, runtimeHeaders("thread-a")...), 400)
	}
	var result struct {
		Events []struct {
			ID int64     `json:"id"`
			At time.Time `json:"at"`
			agentcfg.HookActivity
		}
	}
	view := "/api/v1/execution/sessions/thread-a/activity"
	require.Equal(t, f.client.do("GET", view, "", &result), 200)
	require.Len(t, result.Events, 1)
	assert.Equal(t, result.Events[0].Client, "codex")
	assert.Equal(t, result.Events[0].Event, "PostToolUse")
	assert.Equal(t, *result.Events[0].DurationMS, int64(42))
	rows, err := f.accounts.St.ListEvents(t.Context(), accounts.EventFilter{OrgID: f.org.ID, Action: "agent.hook.", Session: "thread-a", Limit: 100})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.True(t, json.Valid([]byte(rows[0].Detail)))
	other, otherOrg, err := f.accounts.SignUp(t.Context(), "other-activity@example.com", "Other", pw)
	require.NoError(t, err)
	key, _, err := f.accounts.CreateKey(t.Context(), accounts.Principal{User: other, Via: "session"}, otherOrg.ID, "other", 0)
	require.NoError(t, err)
	originalAuth := f.client.auth
	f.client.auth = "Bearer " + key
	assert.Equal(t, f.client.do("GET", view, "", nil), 404)
	assert.Equal(t, f.client.do("POST", path, body, nil, runtimeHeaders("thread-a")...), 403)
	f.client.auth = originalAuth
	require.Equal(t, f.client.do("DELETE", "/api/v1/execution/sessions/thread-a", "", nil), 204)
	assert.Equal(t, f.client.do("GET", view, "", nil), 200, "metadata remains visible to its participant after closure")
	assert.Equal(t, f.client.do("POST", path, body, nil, runtimeHeaders("thread-a")...), 403)
}

func TestHookTelemetryKeepsSessionIdentityOutOfMetrics(t *testing.T) {
	recorder := recordSpans(t)
	f := newRuntimeFixture(t, Config{})
	body := `{"client":"cursor","event":"preCompact","category":"none","outcome":"unknown","context_tokens":120000,"context_usage_percent":85,"message_count":45}`
	require.Equal(t, f.client.do("POST", "/api/v1/execution/activity", body, nil, runtimeHeaders("thread-a")...), 201)
	var output bytes.Buffer
	require.NoError(t, telemetry.Default.WriteText(&output))
	found := false
	for _, line := range strings.Split(output.String(), "\n") {
		if !strings.HasPrefix(line, "agent_hook_events_total{") {
			continue
		}
		if !strings.Contains(line, f.org.ID) {
			continue
		}
		found = true
		assert.Contains(t, line, `mcp_tool="none"`)
		assert.Contains(t, line, `llm_model="none"`)
		assert.False(t, strings.Contains(line, "thread-a"))
		assert.False(t, strings.Contains(line, "session_id="))
		assert.False(t, strings.Contains(line, f.user.ID))
	}
	assert.True(t, found, "activity metrics retain the authenticated tenant")
	spanFound := false
	for _, span := range recorder.Ended() {
		if span.Name() != "agent.hook" {
			continue
		}
		spanFound = true
		attributes := map[string]any{}
		for _, a := range span.Attributes() {
			attributes[string(a.Key)] = a.Value.AsInterface()
		}
		assert.Equal(t, attributes["execution.session_id"], any("thread-a"))
		assert.Equal(t, attributes["agent.context_tokens"], any(int64(120000)))
		assert.Equal(t, attributes["agent.context_usage_percent"], any(float64(85)))
	}
	assert.True(t, spanFound, "opaque session identity links traces to stored activity")
}
