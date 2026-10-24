package accountstest

import (
	"context"
	"errors"
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// RunProfile checks a person's own account changes: name, email and
// sessions, signed-in sessions only, the same on every store.
func RunProfile(t *testing.T, newStore func(*testing.T) accounts.Store) {
	t.Helper()
	ctx := context.Background()
	t.Run("profile", func(t *testing.T) {
		st := newStore(t)
		svc := accounts.NewService(st, true)
		_, err := Bootstrap(ctx, svc)
		require.NoError(t, err)
		_, _, err = svc.SignUp(ctx, "ada@example.com", "Ada", pw)
		require.NoError(t, err)
		_, _, err = svc.SignUp(ctx, "bob@example.com", "Bob", pw)
		require.NoError(t, err)
		login := func(ua string) (string, accounts.Principal) {
			t.Helper()
			lctx := accounts.WithRequestInfo(ctx, accounts.RequestInfo{UserAgent: ua})
			tok, _, err := svc.Login(lctx, "ada@example.com", pw, "test")
			require.NoError(t, err)
			p, err := svc.Authenticate(ctx, tok)
			require.NoError(t, err)
			return tok, p
		}
		_, laptop := login("Firefox on Linux")
		phoneTok, _ := login("Safari on iPhone\x00")

		u, err := svc.UpdateName(ctx, laptop, "  Ada   Lovelace ")
		require.NoError(t, err)
		assert.Equal(t, u.Name, "Ada Lovelace")
		_, err = svc.UpdateName(ctx, laptop, "   ")
		assert.True(t, errors.Is(err, accounts.ErrInvalidInput))
		_, err = svc.UpdateName(ctx, accounts.Principal{User: laptop.User, Via: "key"}, "Agent")
		assert.True(t, errors.Is(err, accounts.ErrForbidden), "an API key changes no account")

		list, err := svc.Sessions(ctx, laptop)
		require.NoError(t, err)
		require.Len(t, list, 2)
		devices := map[string]bool{}
		for _, s := range list {
			devices[s.Device] = s.Current
		}
		assert.Equal(t, devices, map[string]bool{"Firefox on Linux": true, "Safari on iPhone": false}, "control characters are dropped")

		_, err = svc.ChangeEmail(ctx, laptop, "wrong password", "ada@new.example")
		assert.True(t, errors.Is(err, accounts.ErrUnauthorized))
		_, err = svc.ChangeEmail(ctx, laptop, pw, "bob@example.com")
		assert.True(t, errors.Is(err, accounts.ErrExists), "another account's email")
		_, err = svc.ChangeEmail(ctx, laptop, pw, "not an email")
		assert.True(t, errors.Is(err, accounts.ErrInvalidInput))
		u, err = svc.ChangeEmail(ctx, laptop, pw, " Ada@New.Example ")
		require.NoError(t, err)
		assert.Equal(t, u.Email, "ada@new.example")
		_, err = svc.Authenticate(ctx, phoneTok)
		assert.Error(t, err, "a new email ends the other sessions")
		list, err = svc.Sessions(ctx, laptop)
		require.NoError(t, err)
		require.Len(t, list, 1)
		assert.True(t, list[0].Current)

		// A second browser, ended by its ID, then everything but here.
		tok2, _, err := svc.Login(ctx, "ada@new.example", pw, "test")
		require.NoError(t, err)
		list, err = svc.Sessions(ctx, laptop)
		require.NoError(t, err)
		require.Len(t, list, 2)
		var other string
		for _, s := range list {
			if !s.Current {
				other = s.ID
			}
		}
		require.NoError(t, svc.EndSession(ctx, laptop, other))
		_, err = svc.Authenticate(ctx, tok2)
		assert.Error(t, err)
		assert.True(t, errors.Is(svc.EndSession(ctx, laptop, "nope"), accounts.ErrNotFound))
		_, _, err = svc.Login(ctx, "ada@new.example", pw, "test")
		require.NoError(t, err)
		require.NoError(t, svc.EndOtherSessions(ctx, laptop))
		list, err = svc.Sessions(ctx, laptop)
		require.NoError(t, err)
		assert.Len(t, list, 1)

		// Bob cannot end Ada's session.
		bobTok, _, err := svc.Login(ctx, "bob@example.com", pw, "test")
		require.NoError(t, err)
		bob, err := svc.Authenticate(ctx, bobTok)
		require.NoError(t, err)
		assert.True(t, errors.Is(svc.EndSession(ctx, bob, list[0].ID), accounts.ErrNotFound))
	})
}
