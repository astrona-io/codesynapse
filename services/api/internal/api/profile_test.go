package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestProfileEndpoints(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	ts := httptest.NewServer(New(ctx, Config{Accounts: acc}, memory.New(), slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)
	ada := newClient(t, ts)
	require.Equal(t, ada.signUp(`{"email":"ada@example.com","name":"Ada","password":"`+pw+`"}`, nil), 202)

	var u accounts.User
	require.Equal(t, ada.do("PATCH", "/api/v1/me", `{"name":"Ada Lovelace"}`, &u), 200)
	assert.Equal(t, u.Name, "Ada Lovelace")
	assert.Equal(t, ada.do("PATCH", "/api/v1/me", `{"name":""}`, nil), 400)

	var sessions struct {
		Sessions []accounts.SessionView `json:"sessions"`
	}
	require.Equal(t, ada.do("GET", "/api/v1/me/sessions", "", &sessions), 200)
	require.Len(t, sessions.Sessions, 1)
	assert.True(t, sessions.Sessions[0].Current)

	assert.Equal(t, ada.do("POST", "/api/v1/me/email", `{"password":"wrong","email":"ada@new.example"}`, nil), 401)
	var raw string
	require.Equal(t, ada.do("POST", "/api/v1/me/email", `{"password":"`+pw+`","email":"ada@new.example"}`, &raw), 200)
	assert.Contains(t, raw, `"email":"ada@new.example"`)
	assert.False(t, strings.Contains(raw, "argon2"), "no password hash in the answer")

	// An API key changes no account.
	var key struct {
		Secret string `json:"key"`
	}
	var me struct {
		Memberships []accounts.Membership `json:"memberships"`
	}
	require.Equal(t, ada.do("GET", "/api/v1/me", "", &me), 200)
	require.NotEmpty(t, me.Memberships)
	require.Equal(t, ada.do("POST", "/api/v1/me/keys", `{"name":"agent","org":"`+me.Memberships[0].Org.Slug+`"}`, &key), 201)
	agent := newClient(t, ts)
	agent.auth = "Bearer " + key.Secret
	assert.Equal(t, agent.do("PATCH", "/api/v1/me", `{"name":"Agent"}`, nil), 403)

	assert.Equal(t, ada.do("POST", "/api/v1/me/sessions/end-others", "", nil), 204)
	require.Equal(t, ada.do("DELETE", "/api/v1/me/sessions/"+sessions.Sessions[0].ID, "", nil), 204)
	assert.Equal(t, ada.do("GET", "/api/v1/me", "", nil), 401, "ending this session signs out")
}
