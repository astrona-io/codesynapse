package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestMain(m *testing.M) {
	// Sign-ups wait for a floor in production; tests that only need an
	// account do not. TestSignupTakesTheSameTime sets it on its own server.
	defaultSignupFloor = 0
	os.Exit(m.Run())
}

// signUp posts a sign-up and, when it is accepted, signs in with the same
// details, as the sign-up page does. out, when set, receives the user and
// their first organisation. It returns the sign-up status.
func (c *client) signUp(body string, out any, headers ...string) int {
	c.t.Helper()
	code := c.do("POST", "/api/v1/auth/signup", body, nil, headers...)
	if code != http.StatusAccepted {
		return code
	}
	var in struct{ Email, Password string }
	require.NoError(c.t, json.Unmarshal([]byte(body), &in))
	login, err := json.Marshal(map[string]string{"email": in.Email, "password": in.Password})
	require.NoError(c.t, err)
	require.Equal(c.t, c.do("POST", "/api/v1/auth/login", string(login), nil, headers...), http.StatusOK)
	if out != nil {
		var me struct {
			User        accounts.User
			Memberships []accounts.Membership
		}
		require.Equal(c.t, c.do("GET", "/api/v1/me", "", &me), http.StatusOK)
		signed := map[string]any{"user": me.User}
		if len(me.Memberships) > 0 {
			signed["org"] = me.Memberships[0].Org
		}
		b, err := json.Marshal(signed)
		require.NoError(c.t, err)
		require.NoError(c.t, json.Unmarshal(b, out))
	}
	return code
}

// rawSignup answers status, body and whether a session cookie was set.
func rawSignup(t *testing.T, url, body string) (int, string, bool) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/api/v1/auth/signup", strings.NewReader(body)) //nolint:noctx // test
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close() //nolint:errcheck // test
	b, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	cookie := false
	for _, c := range res.Cookies() {
		cookie = cookie || c.Name == SessionCookie
	}
	return res.StatusCode, string(b), cookie
}

func TestSignupDoesNotRevealRegisteredEmails(t *testing.T) {
	ts, acc := accountsServer(t)
	acc.SignupOpen = true
	ada := newClient(t, ts)
	require.Equal(t, ada.signUp(`{"email":"ada@example.com","name":"Ada","username":"ada","password":"`+pw+`"}`, nil), http.StatusAccepted)

	newCode, newBody, newCookie := rawSignup(t, ts.URL, `{"email":"bob@example.com","name":"Bob","password":"`+pw+`"}`)
	takenCode, takenBody, takenCookie := rawSignup(t, ts.URL, `{"email":"ADA@example.com","name":"Eve","password":"another long passphrase"}`)
	assert.Equal(t, newCode, http.StatusAccepted)
	assert.Equal(t, takenCode, newCode, "the same status for a registered email")
	assert.Equal(t, takenBody, newBody, "the same body for a registered email")
	assert.Contains(t, newBody, SignupAccepted)
	assert.False(t, newCookie || takenCookie, "no session either way")

	// The real outcome is audited server-side.
	ctx := context.Background()
	events, err := acc.St.ListEvents(ctx, accounts.EventFilter{Action: "user.signup", Limit: 100})
	require.NoError(t, err)
	refused := false
	for _, e := range events {
		refused = refused || (e.Action == "user.signup" && e.Target == "ada@example.com" && e.Outcome == accounts.Failure && e.Detail == "email already registered")
	}
	assert.True(t, refused, "the registered email is audited as refused")

	// The new account works; the registered one keeps its own password.
	bob := newClient(t, ts)
	assert.Equal(t, bob.do("POST", "/api/v1/auth/login", `{"email":"bob@example.com","password":"`+pw+`"}`, nil), http.StatusOK)
	assert.Equal(t, newClient(t, ts).do("POST", "/api/v1/auth/login", `{"email":"ada@example.com","password":"another long passphrase"}`, nil), http.StatusUnauthorized)

	// A taken username is reported, the same with a free or a registered email.
	freeCode, freeBody, _ := rawSignup(t, ts.URL, `{"email":"carol@example.com","name":"Carol","username":"ada","password":"`+pw+`"}`)
	regCode, regBody, _ := rawSignup(t, ts.URL, `{"email":"ada@example.com","name":"Carol","username":"ada","password":"`+pw+`"}`)
	assert.Equal(t, freeCode, http.StatusConflict)
	assert.Equal(t, regCode, freeCode)
	assert.Equal(t, regBody, freeBody)
}

func TestSignupTakesTheSameTime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	srv := New(ctx, Config{Accounts: acc}, memory.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.signupFloor = 300 * time.Millisecond
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	for _, body := range []string{
		`{"email":"ada@example.com","name":"Ada","password":"` + pw + `"}`,
		`{"email":"ada@example.com","name":"Eve","password":"` + pw + `"}`,
	} {
		start := time.Now()
		code, _, _ := rawSignup(t, ts.URL, body)
		assert.Equal(t, code, http.StatusAccepted)
		assert.True(t, time.Since(start) >= srv.signupFloor, "every answer waits for the floor")
	}
}
