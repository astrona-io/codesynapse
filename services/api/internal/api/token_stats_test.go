package api

import (
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/usage"
)

func TestHookTokensInStats(t *testing.T) {
	ts, _ := newTestServer(t)
	path := "/api/v1/repos/" + storetest.Repo + "/usage/retrieval"
	require.Equal(t, post(t, ts, path, `{"client":"claude-code","tokens":{"input":800,"output":200,"cached_input":600,"turns":2}}`), 204)
	var st Stats
	require.Equal(t, get(t, ts, "/api/v1/stats", &st), 200)
	assert.Equal(t, st.Agents.Tokens, TokenStats{TokenCounts: usage.TokenCounts{Input: 800, Output: 200, CachedInput: 600, Turns: 2}, Total: 1000, InputPct: 80, OutputPct: 20})
	assert.Equal(t, st.Agents.Calls, 0, "reported token counters never inflate MCP calls")
	assert.Len(t, st.Agents.Tools, 0)
	for _, body := range []string{
		`{"client":"codex","tokens":{"input":-1}}`,
		`{"client":"codex","tokens":{"input":1000000001}}`,
		`{"client":"codex","tokens":{"input":1,"cached_input":2}}`,
		`{"client":"codex","tokens":{"turns":100001}}`,
		`{"client":"codex","tokens":{"input":1,"prompt":"private"}}`,
	} {
		assert.Equal(t, post(t, ts, path, body), 400)
	}
}

func TestContextSavingsPercentage(t *testing.T) {
	now := time.Now()
	a := agentStats([]usage.Bucket{{Day: usage.Day(now), Tool: "search", Calls: 1, ResponseTokens: 100}}, 7, now, nil, 4)
	assert.Equal(t, a.AvoidedTokens, 300)
	assert.Equal(t, a.ContextTotalTokens, 400)
	assert.Equal(t, a.ContextSavedPct, float64(75))
	assert.Equal(t, a.Tokens.Total, 0, "MCP response estimates cannot invent model usage")
}
