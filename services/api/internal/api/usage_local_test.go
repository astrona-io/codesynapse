package api

import (
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
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
}
