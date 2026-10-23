package accountstest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// RunSSOStore checks the single sign-on records every store must keep
// identically. newStore returns an empty, migrated store.
func RunSSOStore(t *testing.T, newStore func(t *testing.T) accounts.Store) {
	ctx := context.Background()
	st := newStore(t)
	now := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	hash, err := accounts.HashPassword(pw)
	require.NoError(t, err)
	ada := accounts.User{ID: "u-ada", Email: "ada@example.com", Name: "Ada", CreatedAt: now, PasswordHash: hash}
	require.NoError(t, st.CreateUser(ctx, ada))
	org := accounts.Org{ID: "o-1", Slug: "acme", Name: "Acme", CreatedAt: now}
	require.NoError(t, st.CreateOrg(ctx, org, ada.ID))

	_, err = st.IdentityProvider(ctx, org.ID)
	assert.True(t, errors.Is(err, accounts.ErrNotFound))
	idp := accounts.IdentityProvider{OrgID: org.ID, Name: "Corp", Issuer: "https://idp.example", ClientID: "cs", SecretPreview: "abcd…", KeyID: "k1",
		SealedSecret: []byte{1, 2, 3}, Enabled: true, CreatedBy: ada.ID, UpdatedAt: now,
		IdentityProviderSettings: accounts.IdentityProviderSettings{Scopes: []string{"groups"}, GroupsClaim: "roles", DefaultRole: accounts.Viewer,
			RoleGroups: map[string]accounts.Role{"admins": accounts.Admin}, TeamGroups: map[string]string{"devs": "t-1"}}}
	require.NoError(t, st.PutIdentityProvider(ctx, idp))
	got, err := st.IdentityProvider(ctx, org.ID)
	require.NoError(t, err)
	assert.Equal(t, got, idp)
	idp.Enabled = false
	require.NoError(t, st.PutIdentityProvider(ctx, idp))
	got, err = st.IdentityProvider(ctx, org.ID)
	require.NoError(t, err)
	assert.False(t, got.Enabled, "put replaces")

	link := accounts.ExternalIdentity{ID: "i-1", OrgID: org.ID, Issuer: idp.Issuer, Subject: "sub-1", UserID: ada.ID, Email: ada.Email, LinkedAt: now}
	require.NoError(t, st.PutIdentity(ctx, link))
	byEmail := link
	byEmail.ID = "i-2"
	assert.True(t, errors.Is(st.PutIdentity(ctx, byEmail), accounts.ErrExists), "one account per issuer and subject")
	later := now.Add(time.Hour)
	link.LastLoginAt, link.Email = &later, "new@example.com"
	require.NoError(t, st.PutIdentity(ctx, link))
	found, err := st.IdentityBySubject(ctx, idp.Issuer, "sub-1")
	require.NoError(t, err)
	assert.Equal(t, found, link)
	ids, err := st.UserIdentities(ctx, ada.ID)
	require.NoError(t, err)
	assert.Len(t, ids, 1)
	assert.True(t, errors.Is(st.DeleteIdentity(ctx, "someone-else", link.ID), accounts.ErrNotFound), "only the owner's identity")
	require.NoError(t, st.DeleteIdentity(ctx, ada.ID, link.ID))
	_, err = st.IdentityBySubject(ctx, idp.Issuer, "sub-1")
	assert.True(t, errors.Is(err, accounts.ErrNotFound))

	state := accounts.SSOState{StateHash: "h1", BinderHash: "b1", OrgID: org.ID, Mode: accounts.SSOLink, UserID: ada.ID, Nonce: "n", Verifier: "v", Next: "/x", ExpiresAt: now.Add(time.Minute)}
	require.NoError(t, st.PutSSOState(ctx, state))
	took, err := st.TakeSSOState(ctx, "h1")
	require.NoError(t, err)
	assert.Equal(t, took, state)
	_, err = st.TakeSSOState(ctx, "h1")
	assert.True(t, errors.Is(err, accounts.ErrNotFound), "a state works once")
	old := state
	old.StateHash, old.Mode, old.UserID, old.ExpiresAt = "h2", accounts.SSOLogin, "", now.Add(-time.Minute)
	require.NoError(t, st.PutSSOState(ctx, old))
	require.NoError(t, st.PruneSSOStates(ctx, now))
	_, err = st.TakeSSOState(ctx, "h2")
	assert.True(t, errors.Is(err, accounts.ErrNotFound), "expired states are pruned")

	bob := accounts.User{ID: "u-bob", Email: "bob@example.com", Name: "Bob", CreatedAt: now}
	require.NoError(t, st.CreateUser(ctx, bob))
	assert.Error(t, st.SetManagedByIDP(ctx, org.ID, bob.ID, true), "only a membership can be managed")
	require.NoError(t, st.SetMember(ctx, org.ID, bob.ID, accounts.Member))
	require.NoError(t, st.SetManagedByIDP(ctx, org.ID, bob.ID, true))
	managed, err := st.ManagedByIDP(ctx, org.ID, bob.ID)
	require.NoError(t, err)
	assert.True(t, managed)
	require.NoError(t, st.RemoveMember(ctx, org.ID, bob.ID))
	require.NoError(t, st.SetMember(ctx, org.ID, bob.ID, accounts.Member))
	managed, err = st.ManagedByIDP(ctx, org.ID, bob.ID)
	require.NoError(t, err)
	assert.False(t, managed, "a membership added again by hand is not the provider's")

	require.NoError(t, st.DeleteIdentityProvider(ctx, org.ID))
	assert.True(t, errors.Is(st.DeleteIdentityProvider(ctx, org.ID), accounts.ErrNotFound))
}
