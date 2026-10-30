package api

import (
	"bytes"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/usage"
)

func TestLocalAnswersAreAuditedWithTheirSession(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	path := "/api/v1/usage/local"
	body := `{"tool":"file_outline","client":"claude-code","repo":"` + storetest.Repo + `","duration_ms":12,"response_tokens":300}`
	assert.Equal(t, f.client.do("POST", path, body, nil), 403, "no runtime session, no local answer")
	assert.Equal(t, f.client.do("POST", path, body, nil, runtimeHeaders("unknown")...), 403)
	require.Equal(t, f.client.do("POST", path, body, nil, runtimeHeaders("thread-a")...), 204)
	// A repository the caller cannot read is dropped, so the git tools of any
	// checkout keep working without naming it.
	require.Equal(t, f.client.do("POST", path, `{"tool":"git_status","client":"claude-code","repo":"someone/else"}`, nil, runtimeHeaders("thread-a")...), 204)
	assert.Equal(t, f.client.do("POST", path, `{"tool":"git_status","client":"claude-code","repo":"../x"}`, nil, runtimeHeaders("thread-a")...), 400)

	rows, err := f.accounts.St.ListEvents(t.Context(), accounts.EventFilter{OrgID: f.org.ID, Action: localAnswerAction, Ascending: true, Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	got := rows[0]
	assert.Equal(t, got.UserID, f.user.ID)
	assert.Equal(t, got.Outcome, accounts.Success)
	require.True(t, got.MCP != nil)
	assert.Equal(t, got.MCP.SessionID, "thread-a")
	assert.Equal(t, got.MCP.Name, "file_outline")
	assert.Equal(t, got.MCP.Repo, storetest.Repo)
	assert.Equal(t, got.MCP.Client, "claude-code")
	assert.Equal(t, got.MCP.Bytes, int64(1200))
	assert.Equal(t, rows[1].MCP.Repo, "")
	assert.Equal(t, rows[1].Target, "git_status")

	var out bytes.Buffer
	require.NoError(t, telemetry.Default.WriteText(&out))
	series := func(outcome, tool string) bool {
		for line := range strings.Lines(out.String()) {
			if strings.HasPrefix(line, `mcp_local_answers_total{outcome="`+outcome+`",tenant_id="`+f.org.ID+`",mcp_tool="`+tool+`"`) {
				return true
			}
		}
		return false
	}
	assert.True(t, series(telemetry.LocalRecorded, "file_outline"), "recorded local answers carry the tool")
	assert.True(t, series(telemetry.LocalRecorded, "git_status"))
	assert.True(t, series(telemetry.LocalRefused, "file_outline"), "a missing or unknown session is a refusal")
	assert.True(t, series(telemetry.LocalRefused, "git_status"), "so is a report that fails validation")
}

// fetch_source's measurement is counted into the organisation's source
// buckets (no repository) and the source instruments; it is accepted for
// fetch_source only and within bounds.
func TestLocalFetchSourceCountsItsSavings(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	path := "/api/v1/usage/local"
	report := func(source string) string {
		return `{"tool":"fetch_source","client":"claude-code","duration_ms":40,"response_tokens":500,"source":` + source + `}`
	}
	h := runtimeHeaders("thread-a")
	require.Equal(t, f.client.do("POST", path, report(`{"result":"miss","raw_tokens":8000,"cleaned_tokens":2000,"extracted_tokens":1500,"reduce_us":4000}`), nil, h...), 204)
	// A local model's extract (tier 2) may take longer than cleaning may.
	require.Equal(t, f.client.do("POST", path, report(`{"result":"hit","raw_tokens":8000,"cleaned_tokens":2000,"extracted_tokens":600,"reduce_us":3000,"extract_us":90000000}`), nil, h...), 204)
	require.Equal(t, f.client.do("POST", path, report(`{"result":"refused"}`), nil, h...), 204)
	// A page that grew in cleaning saves nothing rather than a negative.
	require.Equal(t, f.client.do("POST", path, report(`{"result":"revalidated","raw_tokens":100,"cleaned_tokens":120,"extracted_tokens":120}`), nil, h...), 204)

	for name, body := range map[string]string{
		"other tool":     `{"tool":"git_status","client":"claude-code","source":{"result":"miss"}}`,
		"unknown result": report(`{"result":"stale"}`),
		"negative":       report(`{"result":"miss","raw_tokens":-1}`),
		"too large":      report(`{"result":"miss","raw_tokens":1000001}`),
		"slow":           report(`{"result":"miss","reduce_us":60000001}`),
		"slow extract":   report(`{"result":"miss","extract_us":120000001}`),
		"unknown field":  report(`{"result":"miss","url":"https://example.com/"}`),
	} {
		assert.Equal(t, f.client.do("POST", path, body, nil, h...), 400, name)
	}

	buckets, err := f.store.Usage(t.Context(), "")
	require.NoError(t, err)
	counted := map[string]int{}
	for _, b := range buckets {
		if usage.IsWorkflow(b.Tool) {
			assert.Equal(t, b.Repo, "", "source counts carry no repository")
			assert.Equal(t, b.Org, f.org.ID)
		}
		counted[b.Tool] += b.Calls
	}
	assert.Equal(t, counted[usage.SourceFetches], 3, "pages returned; the refused one is not")
	assert.Equal(t, counted[usage.SourceHits], 2, "hit and revalidated")
	assert.Equal(t, counted[usage.SourceRaw], 16100)
	assert.Equal(t, counted[usage.SourceSaved], 6500+7400)
	assert.Equal(t, counted["fetch_source"], 4, "every call is a call")

	var out bytes.Buffer
	require.NoError(t, telemetry.Default.WriteText(&out))
	text := out.String()
	for _, want := range []string{
		`codesynapse_source_fetches_total{result="miss",tenant_id="` + f.org.ID + `",mcp_tool="fetch_source"`,
		`codesynapse_source_fetches_total{result="refused",tenant_id="` + f.org.ID + `"`,
		`codesynapse_source_tokens_total{stage="raw",tenant_id="` + f.org.ID + `"`,
		`codesynapse_source_tokens_total{stage="extracted",tenant_id="` + f.org.ID + `"`,
		`codesynapse_source_reduce_seconds_count{tier="cpu",tenant_id="` + f.org.ID + `"`,
	} {
		assert.Contains(t, text, want)
	}
	assert.NotContains(t, text, "example.com")
}
