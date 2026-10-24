package accountstest

import (
	"context"
	"errors"
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// RunUsername checks the optional username: validation, uniqueness, and
// sign-in by email or username, the same on every store.
func RunUsername(t *testing.T, newStore func(*testing.T) accounts.Store) {
	t.Helper()
	ctx := context.Background()
	t.Run("username", func(t *testing.T) {
		svc := accounts.NewService(newStore(t), true)
		_, err := Bootstrap(ctx, svc)
		require.NoError(t, err)

		ada, _, err := svc.SignUpWith(ctx, accounts.SignUpInput{Email: "ada@example.com", Username: "  Ada.L ", Name: "Ada", Password: pw})
		require.NoError(t, err)
		assert.Equal(t, ada.Username, "ada.l")

		for _, bad := range []string{"ab", "has space", "with@at", "-lead", "x" + string(make([]byte, 40))} {
			_, _, err := svc.SignUpWith(ctx, accounts.SignUpInput{Email: "bad@example.com", Username: bad, Name: "Bad", Password: pw})
			assert.True(t, errors.Is(err, accounts.ErrInvalidInput), "username %q is refused", bad)
		}
		_, _, err = svc.SignUpWith(ctx, accounts.SignUpInput{Email: "other@example.com", Username: "ADA.l", Name: "Other", Password: pw})
		assert.True(t, errors.Is(err, accounts.ErrExists), "usernames are unique, ignoring case")

		for _, login := range []string{"ada@example.com", "ADA@example.com", "ada.l", " Ada.L "} {
			_, u, err := svc.Login(ctx, login, pw, "test")
			require.NoError(t, err)
			assert.Equal(t, u.ID, ada.ID)
		}
		_, _, err = svc.Login(ctx, "ada.l", "wrong password!!", "test")
		assert.True(t, errors.Is(err, accounts.ErrUnauthorized))
		_, _, err = svc.Login(ctx, "nobody", pw, "test")
		assert.True(t, errors.Is(err, accounts.ErrUnauthorized))
		_, _, err = svc.Login(ctx, "", pw, "test")
		assert.True(t, errors.Is(err, accounts.ErrUnauthorized), "an empty login matches nobody")

		// Accounts without a username are not found by the empty name.
		bob, _, err := svc.SignUp(ctx, "bob@example.com", "Bob", pw)
		require.NoError(t, err)
		assert.Equal(t, bob.Username, "")

		tok, _, err := svc.Login(ctx, "bob@example.com", pw, "test")
		require.NoError(t, err)
		p, err := svc.Authenticate(ctx, tok)
		require.NoError(t, err)
		_, err = svc.UpdateUsername(ctx, p, "ada.l")
		assert.True(t, errors.Is(err, accounts.ErrExists))
		u, err := svc.UpdateUsername(ctx, p, "Bobby")
		require.NoError(t, err)
		assert.Equal(t, u.Username, "bobby")
		_, err = svc.UpdateUsername(ctx, accounts.Principal{User: p.User, Via: "key"}, "agent")
		assert.True(t, errors.Is(err, accounts.ErrForbidden), "an API key cannot change it")
		u, err = svc.UpdateUsername(ctx, p, "")
		require.NoError(t, err)
		assert.Equal(t, u.Username, "")
		_, _, err = svc.Login(ctx, "bobby", pw, "test")
		assert.True(t, errors.Is(err, accounts.ErrUnauthorized), "a removed username no longer signs in")
	})
}
