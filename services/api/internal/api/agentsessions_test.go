package api

import (
	"context"
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestAgentSessions: members see their own agent sessions and calls, owners
// and admins the organisation's; other organisations see nothing.
func TestAgentSessions(t *testing.T) {
	ts, acc := accountsServer(t)
	acc.SignupOpen = true
	ctx := context.Background()
	signup := func(email, name string) (*client, accounts.User) {
		c := newClient(t, ts)
		require.Equal(t, c.signUp(`{"email":"`+email+`","name":"`+name+`","password":"`+pw+`"}`, nil), 202)
		var me struct{ User accounts.User }
		require.Equal(t, c.do("GET", "/api/v1/me", "", &me), 200)
		return c, me.User
	}
	ada, adaUser := signup("ada@example.com", "Ada")
	bob, bobUser := signup("bob@example.com", "Bob")
	carol, _ := signup("carol@example.com", "Carol")
	var org accounts.Org
	require.Equal(t, ada.do("POST", "/api/v1/accounts/orgs", `{"name":"Sessions"}`, &org), 201)
	require.Equal(t, ada.do("PUT", "/api/v1/accounts/orgs/"+org.Slug+"/members", `{"email":"bob@example.com","role":"member"}`, nil), 200)

	var made struct {
		Key    string
		APIKey accounts.APIKey `json:"api_key"`
	}
	require.Equal(t, ada.do("POST", "/api/v1/me/keys", `{"org":"`+org.Slug+`","name":"laptop-agent"}`, &made), 201)
	keyID := made.APIKey.ID
	require.True(t, keyID != "", "the key has an ID")

	call := func(u accounts.User, orgID, session, tool string, failed bool) {
		t.Helper()
		e := accounts.Event{UserID: u.ID, ActorEmail: u.Email, OrgID: orgID, Via: "key", KeyID: map[bool]string{true: keyID}[u.ID == adaUser.ID], Action: "mcp.tools.call", Target: tool, Outcome: accounts.Success,
			MCP: &accounts.MCPRequest{Method: "tools/call", Name: tool, Repo: "acme/api", SessionID: session, Client: "claude-code 2.1", Bytes: 1000, DurationMS: 12, Args: map[string]string{"task": "add a field"}}}
		if failed {
			e.Outcome, e.MCP.Error = accounts.Failure, "no exact match"
		}
		_, err := acc.Record(ctx, e)
		require.NoError(t, err)
	}
	call(adaUser, org.ID, "chat-a", "minimal_context", false)
	call(adaUser, org.ID, "chat-a", "get_symbol_context", true)
	call(adaUser, org.ID, "chat-a", "get_symbol_context", false)
	call(bobUser, org.ID, "chat-b", "search", false)
	call(adaUser, "another-org", "chat-x", "search", false)

	base := "/api/v1/accounts/orgs/" + org.Slug + "/agent-sessions"
	type list struct {
		Sessions []AgentSession
		Scope    string
	}
	var all list
	require.Equal(t, ada.do("GET", base, "", &all), 200)
	assert.Equal(t, all.Scope, "organisation", "the owner sees everyone's")
	require.Len(t, all.Sessions, 2)
	byID := map[string]AgentSession{}
	for _, s := range all.Sessions {
		byID[s.ID] = s
	}
	a := byID["chat-a"]
	assert.Equal(t, a.Calls, 3)
	assert.Equal(t, a.Errors, 1)
	assert.Equal(t, a.Bytes, int64(3000))
	assert.Equal(t, a.Tools, map[string]int{"minimal_context": 1, "get_symbol_context": 2})
	assert.Equal(t, a.Repos, []string{"acme/api"})
	assert.Equal(t, a.Client, "claude-code 2.1")
	assert.Equal(t, a.Actor, "ada@example.com")
	assert.Equal(t, a.Key, "laptop-agent", "the list names the API key the chat used")
	assert.Equal(t, byID["chat-b"].Key, "", "an entry without a key names none")

	var own list
	require.Equal(t, bob.do("GET", base, "", &own), 200)
	assert.Equal(t, own.Scope, "own")
	require.Len(t, own.Sessions, 1)
	assert.Equal(t, own.Sessions[0].ID, "chat-b")

	var detail struct {
		Actor string
		Calls []SessionCall
	}
	require.Equal(t, ada.do("GET", base+"/chat-a", "", &detail), 200)
	require.Len(t, detail.Calls, 3)
	assert.Equal(t, detail.Calls[0].Name, "minimal_context", "oldest first")
	assert.Equal(t, detail.Calls[1].Error, "no exact match")
	assert.Equal(t, detail.Calls[0].Args, map[string]string{"task": "add a field"})
	assert.Equal(t, bob.do("GET", base+"/chat-a", "", nil), 404, "a member cannot open someone else's session")
	assert.Equal(t, ada.do("GET", base+"/chat-b", "", nil), 200, "the owner can")
	assert.Equal(t, ada.do("GET", base+"/chat-x", "", nil), 404, "another organisation's session")
	assert.Equal(t, carol.do("GET", base, "", nil), 404, "outsiders do not see the organisation")
}
