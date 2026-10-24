package api

import (
	"testing"

	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestEditReportsInStats(t *testing.T) {
	ts, _ := newTestServer(t)
	path := "/api/v1/repos/" + storetest.Repo + "/usage/retrieval"
	require.Equal(t, post(t, ts, path, `{"client":"claude-code","edits":{"edits":2,"writes":1,"failed":1,"patch_bytes":100,"write_bytes":200,"baseline_bytes":1000,"avoided_bytes":900}}`), 204)
	var st Stats
	require.Equal(t, get(t, ts, "/api/v1/stats", &st), 200)
	assert.Equal(t, st.Agents.Edits.EstimatedOutputSaved, 225)
	assert.Equal(t, st.Agents.Edits.AvoidedPct, float64(90))
	assert.Equal(t, st.Agents.Calls, 0)
	assert.Len(t, st.Agents.Tools, 0)
	assert.Equal(t, post(t, ts, path, `{"client":"codex","edits":{"baseline_bytes":1,"avoided_bytes":2}}`), 400)
	assert.Equal(t, post(t, ts, path, `{"client":"codex","edits":{"edits":1,"failed":2}}`), 400)
	assert.Equal(t, post(t, ts, path, `{"client":"codex","edits":{"edits":1,"source":"private"}}`), 400)
}
