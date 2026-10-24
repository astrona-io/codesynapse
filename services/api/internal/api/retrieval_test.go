package api

import (
	"testing"

	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestRetrievalReport(t *testing.T) {
	ts, _ := newTestServer(t)
	path := "/api/v1/repos/" + storetest.Repo + "/usage/retrieval"
	require.Equal(t, post(t, ts, path, `{"client":"claude-code","tools":{"get_symbol_context":{"returned":4,"read":3},"search":{"returned":2,"read":0}}}`), 204)
	require.Equal(t, post(t, ts, path, `{"client":"claude-code","tools":{"get_symbol_context":{"returned":1,"read":1}}}`), 204)

	var st Stats
	require.Equal(t, get(t, ts, "/api/v1/stats", &st), 200)
	byTool := map[string]ToolStat{}
	for _, tool := range st.Agents.Tools {
		byTool[tool.Tool] = tool
	}
	assert.Equal(t, byTool["get_symbol_context"].RangesReturned, 5)
	assert.Equal(t, byTool["get_symbol_context"].RangesRead, 4)
	assert.Equal(t, byTool["search"].RangesReturned, 2)
	assert.Equal(t, st.Agents.Calls, 0, "a report counts ranges, not calls")

	for _, body := range []string{
		`{"client":"Claude Code","tools":{"search":{"returned":1,"read":0}}}`,
		`{"client":"claude-code","tools":{}}`,
		`{"client":"claude-code","tools":{"rm_rf":{"returned":1,"read":0}}}`,
		`{"client":"claude-code","tools":{"search":{"returned":0,"read":10001}}}`,
		`{"client":"claude-code","tools":{"search":{"returned":-1,"read":0}}}`,
		`{"client":"claude-code","tools":{"search":{"returned":10001,"read":0}}}`,
		`{"client":"claude-code","tools":{"search":{"returned":1,"read":0,"paths":["a.go"]}}}`,
	} {
		assert.Equal(t, post(t, ts, path, body), 400, body)
	}
	assert.Equal(t, post(t, ts, "/api/v1/repos/nope/usage/retrieval", `{"client":"claude-code","tools":{"search":{"returned":1,"read":0}}}`), 404)
}
