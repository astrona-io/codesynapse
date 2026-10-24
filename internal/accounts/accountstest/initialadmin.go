package accountstest

import (
	"context"
	"errors"
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// RunInitialAdmin checks the administrator made at first start: creation,
// sign-in by username or email, replacement by the operator's own root
// (before and after setup), and the operator reset, the same on every store.
func RunInitialAdmin(t *testing.T, newStore func(*testing.T) accounts.Store) {
	t.Helper()
	ctx := context.Background()
	signIn := func(t *testing.T, svc *accounts.Service, login, password string) accounts.Principal {
		t.Helper()
		tok, _, err := svc.Login(ctx, login, password, "test")
		require.NoError(t, err)
		p, err := svc.Authenticate(ctx, tok)
		require.NoError(t, err)
		return p
	}

	t.Run("created once with a generated password", func(t *testing.T) {
		svc := accounts.NewService(newStore(t), false)
		u, generated, err := svc.EnsureInitialAdmin(ctx, "")
		require.NoError(t, err)
		assert.True(t, generated != "", "a password is generated")
		assert.True(t, u.BootstrapRoot && u.PlatformAdmin && u.AutoCreated, "root, platform admin, auto-created")
		assert.Equal(t, u.Username, accounts.InitialAdminUsername)
		again, pw2, err := svc.EnsureInitialAdmin(ctx, "")
		require.NoError(t, err)
		assert.Equal(t, again.ID, "")
		assert.Equal(t, pw2, "")
		active, err := svc.InitialAdminActive(ctx)
		require.NoError(t, err)
		assert.True(t, active)
		signIn(t, svc, "admin", generated)
		signIn(t, svc, accounts.InitialAdminEmail, generated)
		needs, err := svc.Status(ctx)
		require.NoError(t, err)
		assert.True(t, !needs, "an installation with the admin needs no first sign-up")
	})

	t.Run("supplied password is used and weak ones are refused", func(t *testing.T) {
		svc := accounts.NewService(newStore(t), false)
		_, _, err := svc.EnsureInitialAdmin(ctx, "short")
		assert.True(t, errors.Is(err, accounts.ErrInvalidInput))
		u, generated, err := svc.EnsureInitialAdmin(ctx, pw)
		require.NoError(t, err)
		assert.Equal(t, generated, "")
		assert.True(t, u.ID != "")
		signIn(t, svc, "admin", pw)
	})

	t.Run("replaced before and after setup", func(t *testing.T) {
		for _, complete := range []bool{false, true} {
			svc := accounts.NewService(newStore(t), false)
			_, generated, err := svc.EnsureInitialAdmin(ctx, "")
			require.NoError(t, err)
			old := signIn(t, svc, "admin", generated)
			if complete {
				_, err := svc.CompleteSetup(ctx, old, accounts.SetupInput{OrgName: "Acme", Email: "owner@example.com", Name: "Owner", Password: pw})
				require.NoError(t, err)
				old = signIn(t, svc, "admin", generated) // setup ended the session
			}

			_, err = svc.ReplaceRoot(ctx, accounts.Principal{User: old.User, Via: "key"}, accounts.SignUpInput{Email: "me@example.com", Name: "Me", Password: pw})
			assert.True(t, errors.Is(err, accounts.ErrForbidden), "a key cannot replace root")
			_, err = svc.ReplaceRoot(ctx, old, accounts.SignUpInput{Email: "me@example.com", Name: "Me", Password: "short"})
			assert.True(t, errors.Is(err, accounts.ErrInvalidInput))

			// The new account may reuse the name "admin": the old one is going.
			nu, err := svc.ReplaceRoot(ctx, old, accounts.SignUpInput{Email: "me@example.com", Username: "admin", Name: "Me", Password: pw})
			require.NoError(t, err)
			assert.True(t, nu.BootstrapRoot && nu.PlatformAdmin && !nu.AutoCreated, "root, platform admin, not auto-created")

			_, _, err = svc.Login(ctx, accounts.InitialAdminEmail, generated, "test")
			assert.True(t, errors.Is(err, accounts.ErrUnauthorized), "the old password no longer works")
			p := signIn(t, svc, "admin", pw)
			assert.Equal(t, p.User.Email, "me@example.com")
			active, err := svc.InitialAdminActive(ctx)
			require.NoError(t, err)
			assert.True(t, !active, "nothing auto-created is left")
			setup, err := svc.St.Setup(ctx)
			require.NoError(t, err)
			assert.Equal(t, setup.RootUserID, nu.ID)

			_, err = svc.ReplaceRoot(ctx, p, accounts.SignUpInput{Email: "again@example.com", Name: "Again", Password: pw})
			assert.True(t, errors.Is(err, accounts.ErrForbidden), "only the auto-created root can be replaced")
			if !complete {
				_, err = svc.CompleteSetup(ctx, p, accounts.SetupInput{OrgName: "Acme", Email: "owner@example.com", Name: "Owner", Password: pw})
				assert.NoError(t, err, "the new root finishes setup")
			}
		}
	})

	t.Run("reset", func(t *testing.T) {
		svc := accounts.NewService(newStore(t), false)
		_, generated, err := svc.ResetRoot(ctx, "")
		require.NoError(t, err) // empty installation: creates the administrator
		old := signIn(t, svc, "admin", generated)

		u, fresh, err := svc.ResetRoot(ctx, "")
		require.NoError(t, err)
		assert.True(t, fresh != "" && fresh != generated)
		assert.Equal(t, u.Username, "admin")
		_, _, err = svc.Login(ctx, "admin", generated, "test")
		assert.True(t, errors.Is(err, accounts.ErrUnauthorized), "the old password is gone")
		assert.True(t, svc.ValidatePrincipal(ctx, old) != nil, "the old session is gone")
		signIn(t, svc, "admin", fresh)

		_, _, err = svc.ResetRoot(ctx, "short")
		assert.True(t, errors.Is(err, accounts.ErrInvalidInput))
		now := svc.Now()
		require.NoError(t, svc.St.SetUserDisabled(ctx, u.ID, &now))
		_, same, err := svc.ResetRoot(ctx, pw)
		require.NoError(t, err)
		assert.Equal(t, same, "")
		signIn(t, svc, "admin", pw)
	})
}
