package accountstest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// RunUserGitTokens covers a person's own Git tokens: managed only by the
// person in a signed-in session, audited, sealed to the person, and opened
// only while they are an active member of the repository's organisation.
func RunUserGitTokens(t *testing.T, st accounts.Store) {
	ctx := context.Background()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	svc := accounts.NewService(st, true)
	svc.Now = func() time.Time { return now }
	user := func(email string) (accounts.User, accounts.Principal) {
		u, _, err := svc.SignUp(ctx, email, email, pw)
		require.NoError(t, err)
		hash := "utok-" + u.ID
		require.NoError(t, st.CreateSession(ctx, accounts.Session{TokenHash: hash, UserID: u.ID, CreatedAt: now, ExpiresAt: sessionExpiry(now)}))
		return u, accounts.Principal{User: u, Via: "session", SessionHash: hash}
	}
	owner, ownerP := user("utok-owner@example.com")
	member, memberP := user("utok-member@example.com")
	org, err := svc.CreateOrg(ctx, ownerP, "Utok Acme", "")
	require.NoError(t, err)
	require.NoError(t, Join(ctx, t, svc, ownerP, org.ID, member, accounts.Member))

	const tok = "ghp_userownabcdefghijklmnopqrstuvwxyz01"
	_, err = svc.SetUserGitToken(ctx, memberP, "github.com", tok)
	assert.True(t, errors.Is(err, accounts.ErrNoSecretKey), "no secret key, no tokens")
	key := make([]byte, 32)
	key[0] = 9
	svc.Secrets, err = accounts.NewSealer(key)
	require.NoError(t, err)

	keyP := accounts.Principal{User: member, Via: "key", OrgID: org.ID}
	_, err = svc.SetUserGitToken(ctx, keyP, "github.com", tok)
	assert.True(t, errors.Is(err, accounts.ErrForbidden), "API keys do not manage secrets")
	_, err = svc.UserGitTokens(ctx, keyP)
	assert.True(t, errors.Is(err, accounts.ErrForbidden), "API keys do not list them either")
	_, err = svc.SetUserGitToken(ctx, memberP, "https://github.com/x", tok)
	assert.True(t, errors.Is(err, accounts.ErrInvalidInput), "a host, not a URL")
	_, err = svc.SetUserGitToken(ctx, memberP, "github.com", "short")
	assert.True(t, errors.Is(err, accounts.ErrInvalidInput))

	set, err := svc.SetUserGitToken(ctx, memberP, " GitHub.com ", tok)
	require.NoError(t, err)
	assert.Equal(t, set.Host, "github.com")
	assert.Equal(t, set.Preview, accounts.Preview(tok))
	rows, err := st.ListEvents(ctx, accounts.EventFilter{Actor: member.ID, Action: "user.git_token.", Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, rows[0].Action, "user.git_token.set")
	assert.False(t, strings.Contains(mustJSON(t, rows), tok), "the audit entry never holds the token")

	listed, err := svc.UserGitTokens(ctx, memberP)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	b := mustJSON(t, listed)
	assert.False(t, strings.Contains(b, tok) || strings.Contains(b, "sealed"), "listing shows the preview only")
	other, err := svc.UserGitTokens(ctx, ownerP)
	require.NoError(t, err)
	assert.Equal(t, len(other), 0, "each person sees only their own tokens")
	stored, err := st.UserGitToken(ctx, member.ID, "github.com")
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(stored.Sealed), tok), "stored sealed, never in clear")

	opened, err := svc.OpenUserGitToken(ctx, org.ID, member.ID, "github.com")
	require.NoError(t, err)
	assert.Equal(t, opened, tok)
	_, err = svc.OpenGitToken(ctx, org.ID, "github.com")
	assert.True(t, errors.Is(err, accounts.ErrNotFound), "a person's token is not the organisation's")
	require.NoError(t, st.PutGitToken(ctx, accounts.GitToken{OrgID: org.ID, Host: "github.com", Preview: stored.Preview, KeyID: stored.KeyID, Sealed: stored.Sealed, CreatedBy: member.ID, UpdatedAt: now}))
	_, err = svc.OpenGitToken(ctx, org.ID, "github.com")
	assert.Error(t, err, "a person's sealed token copied to the organisation does not open")
	stored.UserID = owner.ID
	require.NoError(t, st.PutUserGitToken(ctx, stored))
	_, err = svc.OpenUserGitToken(ctx, org.ID, owner.ID, "github.com")
	assert.Error(t, err, "a sealed token copied to another person does not open")

	// Replacing keeps the creation time; removal is audited and final.
	later := now.Add(time.Hour)
	svc.Now = func() time.Time { return later }
	again, err := svc.SetUserGitToken(ctx, memberP, "github.com", tok+"2")
	require.NoError(t, err)
	assert.True(t, again.CreatedAt.Equal(now), "replacing keeps the creation time")
	stored, err = st.UserGitToken(ctx, member.ID, "github.com")
	require.NoError(t, err)
	assert.True(t, stored.CreatedAt.Equal(now) && stored.UpdatedAt.Equal(later))

	// Leaving the organisation closes the token for its repositories.
	require.NoError(t, st.RemoveMember(ctx, org.ID, member.ID))
	_, err = svc.OpenUserGitToken(ctx, org.ID, member.ID, "github.com")
	assert.True(t, errors.Is(err, accounts.ErrNotFound), "only while a member")

	assert.True(t, errors.Is(svc.RemoveUserGitToken(ctx, keyP, "github.com"), accounts.ErrForbidden))
	require.NoError(t, svc.RemoveUserGitToken(ctx, memberP, "github.com"))
	assert.True(t, errors.Is(svc.RemoveUserGitToken(ctx, memberP, "github.com"), accounts.ErrNotFound))
	rows, err = st.ListEvents(ctx, accounts.EventFilter{Actor: member.ID, Action: "user.git_token.remove", Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, len(rows), 1)
	left, err := st.UserGitTokens(ctx, member.ID)
	require.NoError(t, err)
	assert.Equal(t, len(left), 0)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}
