package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/agentrules"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/usage"
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
		`{"client":"claude-code","workflow":{"first_native":-1}}`,
		`{"client":"claude-code","workflow":{"reads_turned_back":10001}}`,
		`{"client":"claude-code","workflow":{"files_edited":-2}}`,
		`{"client":"claude-code","workflow":{"first_native":1,"path":"a.go"}}`,
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

func TestWorkflowReportShowsInStats(t *testing.T) {
	ts, _ := newTestServer(t)
	path := "/api/v1/repos/" + storetest.Repo + "/usage/retrieval"
	// A session that never used a CodeSynapse tool reports the workflow alone.
	require.Equal(t, post(t, ts, path, `{"client":"claude-code","workflow":{"first_native":1,"reads_turned_back":2}}`), 204)
	require.Equal(t, post(t, ts, path, `{"client":"claude-code","workflow":{"first_codesynapse":3}}`), 204)
	// Edits of sessions: four files changed, three of them pointed to.
	require.Equal(t, post(t, ts, path, `{"client":"claude-code","workflow":{"files_edited":3,"files_edited_found":2}}`), 204)
	require.Equal(t, post(t, ts, path, `{"client":"claude-code","workflow":{"files_edited":1,"files_edited_found":1}}`), 204)
	require.Equal(t, post(t, ts, path, `{"client":"claude-code","workflow":{}}`), 204, "an empty report is accepted and stores nothing")

	var st Stats
	require.Equal(t, get(t, ts, "/api/v1/stats", &st), 200)
	assert.Equal(t, st.Agents.Workflow.FirstCodeSynapse, 3)
	assert.Equal(t, st.Agents.Workflow.FirstNative, 1)
	assert.Equal(t, st.Agents.Workflow.ReadsTurnedBack, 2)
	assert.Equal(t, st.Agents.Workflow.FirstSharePct, 75)
	assert.Equal(t, st.Agents.Workflow.FilesEdited, 4)
	assert.Equal(t, st.Agents.Workflow.FilesEditedFound, 3)
	assert.Equal(t, st.Agents.Workflow.EditRecallPct, 75)
	assert.Equal(t, st.Agents.Calls, 0, "workflow counts are not tool calls")
	assert.Equal(t, len(st.Agents.Tools), 0, "and not tools")
	assert.Equal(t, len(st.Agents.Clients), 0)
}

func TestAgentRulesEndpoint(t *testing.T) {
	ts, _ := newTestServer(t)
	var rules struct {
		Version int    `json:"version"`
		Custom  bool   `json:"custom"`
		Text    string `json:"text"`
	}
	require.Equal(t, get(t, ts, "/api/v1/agent-rules", &rules), 200)
	assert.True(t, !rules.Custom)
	assert.Equal(t, rules.Version, agentrules.Version)
	assert.Equal(t, rules.Text, agentrules.Default)
}

func TestAgentStatusCountsTodaysActivity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	ts := httptest.NewServer(New(ctx, Config{}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)
	var before AgentStatus
	require.Equal(t, get(t, ts, "/api/v1/agents/status", &before), 200)
	assert.Equal(t, before, AgentStatus{})

	today := usage.Day(time.Now())
	require.NoError(t, st.AddUsage(ctx, []usage.Bucket{
		{Day: today, Tool: usage.Session, Client: "claude-code", Calls: 2},
		{Day: today, Repo: storetest.Repo, Tool: "search", Client: "claude-code", Calls: 3},
		{Day: today, Repo: storetest.Repo, Tool: usage.FirstCodeSynapse, Client: "claude-code", Calls: 1},
		{Day: today, Repo: storetest.Repo, Tool: usage.ReadTurnedBack, Client: "claude-code", Calls: 4},
		{Day: usage.Day(time.Now().AddDate(0, 0, -2)), Repo: storetest.Repo, Tool: "search", Client: "claude-code", Calls: 9}, // not today
	}))
	var got AgentStatus
	require.Equal(t, get(t, ts, "/api/v1/agents/status", &got), 200)
	assert.Equal(t, got, AgentStatus{Sessions: 2, Calls: 3, Workflow: 1})
}
