package accountstest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// sessionFor gives u a signed-in session principal, as a browser has.
func sessionFor(ctx context.Context, t testing.TB, svc *accounts.Service, u accounts.User) accounts.Principal {
	t.Helper()
	now := time.Now()
	if svc.Now != nil {
		now = svc.Now()
	}
	hash := accounts.HashToken(fmt.Sprintf("test-session-%s-%d", u.ID, sessionSeq.Add(1)))
	require.NoError(t, svc.St.CreateSession(ctx, accounts.Session{TokenHash: hash, UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(accounts.SessionTTL)}))
	return accounts.Principal{User: u, Via: "session", SessionHash: hash}
}

var sessionSeq atomic.Int64

// Join makes u a member of orgID through an invitation from inviter, the only
// way a new member joins. It returns the first refusal. The sessions it opens
// are closed again, so counts of sessions stay what the caller set up.
func Join(ctx context.Context, t testing.TB, svc *accounts.Service, inviter accounts.Principal, orgID string, u accounts.User, role accounts.Role) error {
	t.Helper()
	if inviter.Via == "session" && inviter.SessionHash == "" {
		inviter = sessionFor(ctx, t, svc, inviter.User)
		defer func() { require.NoError(t, svc.St.DeleteSession(ctx, inviter.SessionHash)) }()
	}
	_, token, err := svc.CreateInvitation(ctx, inviter, orgID, u.Email, role)
	if err != nil {
		return err
	}
	invitee := sessionFor(ctx, t, svc, u)
	defer func() { require.NoError(t, svc.St.DeleteSession(ctx, invitee.SessionHash)) }()
	_, err = svc.AcceptInvitation(ctx, invitee, token)
	return err
}

// auditTxFailing refuses audit entries, also inside policy transactions, so a
// change that must be audited first cannot happen.
type auditTxFailing struct{ accounts.Store }

func (auditTxFailing) AddEvent(context.Context, accounts.Event) (accounts.Event, error) {
	return accounts.Event{}, errors.New("disk full")
}

func (f auditTxFailing) PolicyTransaction(ctx context.Context, org string, fn func(accounts.Store) error) error {
	return f.Store.PolicyTransaction(ctx, org, func(st accounts.Store) error { return fn(auditTxFailing{st}) })
}

// RunInvitations covers joining an organisation by invitation: creating one
// never reveals whether an email has an account, only the invited account
// accepts it in a signed-in session, it works once and for seven days, the
// role never exceeds the inviter's at acceptance, root and suspended accounts
// never accept, and every step is audited before it happens.
func RunInvitations(t *testing.T, newStore func(*testing.T) accounts.Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	setup := func(t *testing.T) (accounts.Store, *accounts.Service, accounts.Principal, accounts.Principal, string) {
		st := newStore(t)
		svc := accounts.NewService(st, true)
		svc.Now = func() time.Time { return now }
		root, err := Bootstrap(ctx, svc)
		require.NoError(t, err)
		owner, err := login(ctx, svc, SetupOwner)
		require.NoError(t, err)
		ms, err := st.UserMemberships(ctx, owner.User.ID)
		require.NoError(t, err)
		require.Len(t, ms, 1)
		return st, svc, root, owner, ms[0].Org.ID
	}
	signUp := func(t *testing.T, svc *accounts.Service, email string) (accounts.User, accounts.Principal) {
		u, _, err := svc.SignUp(ctx, email, email, pw)
		require.NoError(t, err)
		return u, sessionFor(ctx, t, svc, u)
	}
	events := func(t *testing.T, st accounts.Store, action string) []accounts.Event {
		evs, err := st.ListEvents(ctx, accounts.EventFilter{Action: action, Limit: 100})
		require.NoError(t, err)
		return evs
	}

	t.Run("create never looks anyone up", func(t *testing.T) {
		st, svc, _, owner, org := setup(t)
		known, _ := signUp(t, svc, "known@example.com")
		users, err := st.CountUsers(ctx)
		require.NoError(t, err)
		a, tokA, err := svc.CreateInvitation(ctx, owner, org, " Known@Example.com ", accounts.Member)
		require.NoError(t, err)
		b, tokB, err := svc.CreateInvitation(ctx, owner, org, "nobody@example.com", accounts.Member)
		require.NoError(t, err)
		assert.Equal(t, a.Email, known.Email, "emails are normalised")
		assert.Equal(t, a.ExpiresAt, now.Add(accounts.InvitationTTL))
		assert.Equal(t, b.ExpiresAt, now.Add(accounts.InvitationTTL))
		assert.True(t, len(tokA) == len(tokB) && tokA != tokB, "tokens look alike and differ")
		assert.Equal(t, a.TokenHash, accounts.HashToken(tokA), "only the hash is kept")
		after, err := st.CountUsers(ctx)
		require.NoError(t, err)
		assert.Equal(t, after, users, "no account is created")
		evs := events(t, st, "org.invite.create")
		require.Len(t, evs, 2)
		for _, e := range evs {
			assert.False(t, strings.Contains(e.Detail, tokA) || strings.Contains(e.Detail, tokB), "tokens are never audited")
		}
		open, err := svc.Invitations(ctx, owner, org)
		require.NoError(t, err)
		assert.Len(t, open, 2)

		_, _, err = svc.CreateInvitation(ctx, owner, org, "not an email", accounts.Member)
		assert.True(t, errors.Is(err, accounts.ErrInvalidInput))
		_, _, err = svc.CreateInvitation(ctx, owner, org, "x@example.com", "boss")
		assert.True(t, errors.Is(err, accounts.ErrInvalidInput))
	})

	t.Run("who may invite", func(t *testing.T) {
		st, svc, root, owner, org := setup(t)
		admin, adminP := signUp(t, svc, "admin@example.com")
		member, memberP := signUp(t, svc, "member@example.com")
		_, outsiderP := signUp(t, svc, "outsider@example.com")
		require.NoError(t, Join(ctx, t, svc, owner, org, admin, accounts.Admin))
		require.NoError(t, Join(ctx, t, svc, owner, org, member, accounts.Member))

		_, _, err := svc.CreateInvitation(ctx, memberP, org, "x@example.com", accounts.Viewer)
		assert.True(t, errors.Is(err, accounts.ErrForbidden), "members cannot invite")
		_, _, err = svc.CreateInvitation(ctx, outsiderP, org, "x@example.com", accounts.Viewer)
		assert.True(t, errors.Is(err, accounts.ErrForbidden), "outsiders cannot invite")
		_, _, err = svc.CreateInvitation(ctx, root, org, "x@example.com", accounts.Viewer)
		assert.Error(t, err, "root does no organisation work")
		_, _, err = svc.CreateInvitation(ctx, adminP, org, "x@example.com", accounts.Owner)
		assert.True(t, errors.Is(err, accounts.ErrForbidden), "nobody offers a role above their own")
		key := accounts.Principal{User: owner.User, Via: "key", KeyID: "k", OrgID: org}
		_, _, err = svc.CreateInvitation(ctx, key, org, "x@example.com", accounts.Viewer)
		assert.True(t, errors.Is(err, accounts.ErrForbidden), "never with an API key")
		_, err = svc.Invitations(ctx, memberP, org)
		assert.True(t, errors.Is(err, accounts.ErrForbidden))
		_, _, err = svc.CreateInvitation(ctx, adminP, org, "x@example.com", accounts.Admin)
		require.NoError(t, err)
		assert.Len(t, events(t, st, "org.invite.create"), 3, "two joins and one invitation")
	})

	t.Run("accepting", func(t *testing.T) {
		st, svc, root, owner, org := setup(t)
		dana, danaP := signUp(t, svc, "dana@example.com")
		_, eveP := signUp(t, svc, "eve@example.com")
		_, tok, err := svc.CreateInvitation(ctx, owner, org, "DANA@example.com", accounts.Member)
		require.NoError(t, err)

		pre, err := svc.PreviewInvitation(ctx, eveP, tok)
		require.NoError(t, err)
		assert.False(t, pre.ForYou)
		assert.Equal(t, pre.Org.ID, org)
		_, err = svc.AcceptInvitation(ctx, eveP, tok)
		assert.True(t, errors.Is(err, accounts.ErrInvitationEmail), "another account cannot accept")
		_, err = svc.AcceptInvitation(ctx, root, tok)
		assert.Error(t, err, "root never accepts")
		_, err = svc.AcceptInvitation(ctx, accounts.Principal{User: dana, Via: "session"}, tok)
		assert.True(t, errors.Is(err, accounts.ErrForbidden), "a signed-in session is required")
		_, err = svc.AcceptInvitation(ctx, accounts.Principal{User: dana, Via: "key", KeyID: "k", OrgID: org}, tok)
		assert.True(t, errors.Is(err, accounts.ErrForbidden), "never with an API key")
		_, err = svc.AcceptInvitation(ctx, danaP, "wrong")
		assert.True(t, errors.Is(err, accounts.ErrInvitationInvalid))
		_, err = svc.AcceptInvitation(ctx, danaP, strings.Repeat("x", 500))
		assert.True(t, errors.Is(err, accounts.ErrInvitationInvalid))

		pre, err = svc.PreviewInvitation(ctx, danaP, tok)
		require.NoError(t, err)
		assert.True(t, pre.ForYou)
		assert.Equal(t, pre.Role, accounts.Member)
		m, err := svc.AcceptInvitation(ctx, danaP, tok)
		require.NoError(t, err)
		assert.Equal(t, m.Role, accounts.Member)
		role, err := st.MemberRole(ctx, org, dana.ID)
		require.NoError(t, err)
		assert.Equal(t, role, accounts.Member)
		_, err = svc.AcceptInvitation(ctx, danaP, tok)
		assert.True(t, errors.Is(err, accounts.ErrInvitationInvalid), "an invitation works once")
		open, err := svc.Invitations(ctx, owner, org)
		require.NoError(t, err)
		assert.Len(t, open, 0)
		assert.Len(t, events(t, st, "org.invite.accept"), 2, "the refused and the accepted one")

		_, tok, err = svc.CreateInvitation(ctx, owner, org, dana.Email, accounts.Admin)
		require.NoError(t, err)
		_, err = svc.AcceptInvitation(ctx, danaP, tok)
		assert.True(t, errors.Is(err, accounts.ErrExists), "members change roles through the members list")
		role, err = st.MemberRole(ctx, org, dana.ID)
		require.NoError(t, err)
		assert.Equal(t, role, accounts.Member)
	})

	t.Run("suspended accounts never accept", func(t *testing.T) {
		st, svc, _, owner, org := setup(t)
		frank, frankP := signUp(t, svc, "frank@example.com")
		_, tok, err := svc.CreateInvitation(ctx, owner, org, frank.Email, accounts.Member)
		require.NoError(t, err)
		at := now
		require.NoError(t, st.SetUserDisabled(ctx, frank.ID, &at))
		_, err = svc.AcceptInvitation(ctx, frankP, tok)
		assert.Error(t, err)
		_, err = st.MemberRole(ctx, org, frank.ID)
		assert.True(t, errors.Is(err, accounts.ErrNotFound))
	})

	t.Run("role capped at the inviter's role now", func(t *testing.T) {
		st, svc, _, owner, org := setup(t)
		admin, adminP := signUp(t, svc, "capper@example.com")
		gail, gailP := signUp(t, svc, "gail@example.com")
		hal, halP := signUp(t, svc, "hal@example.com")
		require.NoError(t, Join(ctx, t, svc, owner, org, admin, accounts.Owner))
		_, tok, err := svc.CreateInvitation(ctx, adminP, org, gail.Email, accounts.Owner)
		require.NoError(t, err)
		_, tokHal, err := svc.CreateInvitation(ctx, adminP, org, hal.Email, accounts.Member)
		require.NoError(t, err)
		_, err = svc.SetMemberRole(ctx, owner, org, admin.ID, accounts.Admin)
		require.NoError(t, err)
		m, err := svc.AcceptInvitation(ctx, gailP, tok)
		require.NoError(t, err)
		assert.Equal(t, m.Role, accounts.Admin, "invited as owner by someone who is now an admin")
		evs := events(t, st, "org.invite.accept")
		require.Len(t, evs, 2)
		assert.True(t, strings.Contains(evs[0].Detail, "capped"), evs[0].Detail)

		_, err = svc.SetMemberRole(ctx, owner, org, admin.ID, accounts.Member)
		require.NoError(t, err)
		_, err = svc.AcceptInvitation(ctx, halP, tokHal)
		assert.True(t, errors.Is(err, accounts.ErrInviterGone), "an inviter who can no longer add members")
		_, err = st.MemberRole(ctx, org, hal.ID)
		assert.True(t, errors.Is(err, accounts.ErrNotFound))
	})

	t.Run("expiry, revocation and the limit", func(t *testing.T) {
		st, svc, _, owner, org := setup(t)
		ivy, ivyP := signUp(t, svc, "ivy@example.com")
		inv, tok, err := svc.CreateInvitation(ctx, owner, org, ivy.Email, accounts.Member)
		require.NoError(t, err)
		require.NoError(t, svc.RevokeInvitation(ctx, owner, org, inv.ID))
		_, err = svc.AcceptInvitation(ctx, ivyP, tok)
		assert.True(t, errors.Is(err, accounts.ErrInvitationInvalid), "revoked")
		assert.True(t, errors.Is(svc.RevokeInvitation(ctx, owner, org, inv.ID), accounts.ErrNotFound))
		assert.Len(t, events(t, st, "org.invite.revoke"), 1)

		_, tok, err = svc.CreateInvitation(ctx, owner, org, ivy.Email, accounts.Member)
		require.NoError(t, err)
		saved := now
		now = now.Add(accounts.InvitationTTL)
		_, err = svc.PreviewInvitation(ctx, ivyP, tok)
		assert.True(t, errors.Is(err, accounts.ErrInvitationInvalid), "expired")
		_, err = svc.AcceptInvitation(ctx, ivyP, tok)
		assert.True(t, errors.Is(err, accounts.ErrInvitationInvalid), "expired")
		assert.Len(t, events(t, st, "org.invite.expire"), 1, "expired once, audited once")
		now = saved

		for i := range accounts.MaxPendingInvitations {
			_, _, err := svc.CreateInvitation(ctx, owner, org, "p"+string(rune('a'+i%26))+string(rune('a'+i/26))+"@example.com", accounts.Viewer)
			require.NoError(t, err)
		}
		_, _, err = svc.CreateInvitation(ctx, owner, org, "over@example.com", accounts.Viewer)
		assert.True(t, errors.Is(err, accounts.ErrInvitationLimit))
		now = now.Add(accounts.InvitationTTL + time.Minute)
		_, _, err = svc.CreateInvitation(ctx, owner, org, "over@example.com", accounts.Viewer)
		require.NoError(t, err, "expired invitations free their place")
		now = saved
	})

	t.Run("no change without an audit entry", func(t *testing.T) {
		st, svc, _, owner, org := setup(t)
		jo, joP := signUp(t, svc, "jo@example.com")
		broken := accounts.NewService(auditTxFailing{st}, true)
		broken.Now = svc.Now
		_, _, err := broken.CreateInvitation(ctx, owner, org, jo.Email, accounts.Member)
		assert.Error(t, err)
		open, err := svc.Invitations(ctx, owner, org)
		require.NoError(t, err)
		assert.Len(t, open, 0, "no invitation without its audit entry")

		inv, tok, err := svc.CreateInvitation(ctx, owner, org, jo.Email, accounts.Member)
		require.NoError(t, err)
		_, err = broken.AcceptInvitation(ctx, joP, tok)
		assert.Error(t, err)
		_, err = st.MemberRole(ctx, org, jo.ID)
		assert.True(t, errors.Is(err, accounts.ErrNotFound), "no membership without its audit entry")
		assert.Error(t, broken.RevokeInvitation(ctx, owner, org, inv.ID))
		_, err = svc.AcceptInvitation(ctx, joP, tok)
		require.NoError(t, err, "the invitation is still there")
	})
}
