package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestAPIAccess: the MCP server asks the API with the calling agent's own key
// and runtime session, never the installation token, and refuses on failure.
func TestAPIAccess(t *testing.T) {
	var seen http.Header
	status := http.StatusOK
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		assert.Equal(t, r.URL.Path, "/api/v1/internal/access")
		var q accounts.AccessQuery
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&q))
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(accounts.Access{UserID: "u", Repos: []accounts.RepoAccess{{OrgID: q.OrgID, Graph: "g"}}}) //nolint:errcheck // test
	}))
	t.Cleanup(ts.Close)
	a := APIAccess{API: &APIClient{BaseURL: ts.URL, Token: "installation-token"}}
	id := execution.Identity{TenantID: "o", SubjectID: "u", SessionID: "chat", ParentSessionID: "chat"}
	ctx, err := execution.With(t.Context(), id)
	require.NoError(t, err)
	caller := &Caller{UserID: "u", OrgID: "o", KeyID: "k", token: "csk_caller"}
	p := accounts.Principal{User: accounts.User{ID: "u"}, Via: "key", KeyID: "k", OrgID: "o"}

	got, err := a.AccessIn(context.WithValue(ctx, callerKey{}, caller), p, accounts.AccessQuery{OrgID: "o"})
	require.NoError(t, err)
	require.Equal(t, len(got.Repos), 1)
	assert.Equal(t, seen.Get("Authorization"), "Bearer csk_caller")
	assert.Equal(t, seen.Get(execution.SessionHeader), "chat")

	seen = nil
	_, err = a.AccessIn(ctx, p, accounts.AccessQuery{OrgID: "o"})
	assert.True(t, errors.Is(err, accounts.ErrForbidden), "no caller, no question")
	assert.True(t, seen == nil, "the installation token is never used")

	_, err = a.AccessIn(context.WithValue(ctx, callerKey{}, caller), accounts.Principal{User: accounts.User{ID: "v"}}, accounts.AccessQuery{OrgID: "o"})
	assert.True(t, errors.Is(err, accounts.ErrForbidden), "only for the caller")

	status = http.StatusForbidden
	_, err = a.AccessIn(context.WithValue(ctx, callerKey{}, caller), p, accounts.AccessQuery{OrgID: "o"})
	assert.True(t, errors.Is(err, accounts.ErrForbidden), "a refusal refuses")
}
