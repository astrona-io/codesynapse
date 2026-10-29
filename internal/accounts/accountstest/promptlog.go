package accountstest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// RunPromptLog covers the opt-in gateway prompt log: off by default and then
// keeping nothing; switched on only by an admin in a signed-in session and
// audited; rows redacted and capped; read only by admins in a signed-in
// session, each read audited; per organisation; pruned and sealed.
func RunPromptLog(t *testing.T, newStore func(*testing.T) accounts.Store) {
	ctx := context.Background()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	setup := func(t *testing.T) (accounts.Store, *accounts.Service) {
		st := newStore(t)
		for _, id := range []string{"pl-owner", "pl-member", "pl-other"} {
			require.NoError(t, st.CreateUser(ctx, accounts.User{ID: id, Name: id, Email: id + "@example.com", CreatedAt: now}))
			require.NoError(t, st.CreateSession(ctx, accounts.Session{TokenHash: "h-" + id, UserID: id, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}))
		}
		require.NoError(t, st.CreateOrg(ctx, accounts.Org{ID: "pl-org-a", Name: "a", Slug: "pl-org-a", CreatedAt: now}, "pl-owner"))
		require.NoError(t, st.CreateOrg(ctx, accounts.Org{ID: "pl-org-b", Name: "b", Slug: "pl-org-b", CreatedAt: now}, "pl-other"))
		require.NoError(t, st.SetMember(ctx, "pl-org-a", "pl-member", accounts.Member))
		svc := accounts.NewService(st, false)
		svc.Now = func() time.Time { return now }
		return st, svc
	}
	owner := accounts.Principal{User: accounts.User{ID: "pl-owner"}, Via: "session", SessionHash: "h-pl-owner"}
	member := accounts.Principal{User: accounts.User{ID: "pl-member"}, Via: "session", SessionHash: "h-pl-member"}
	other := accounts.Principal{User: accounts.User{ID: "pl-other"}, Via: "session", SessionHash: "h-pl-other"}
	key := accounts.Principal{User: accounts.User{ID: "pl-owner"}, Via: "key", OrgID: "pl-org-a"}
	secret := "AKIA" + "ABCDEFGHIJKLMNOP"
	entry := func(org, prompt string) accounts.PromptLogEntry {
		return accounts.PromptLogEntry{
			OrgID: org, At: now, AuditID: 7, UserID: "pl-owner", KeyID: "k1", SessionID: "s1", Path: "/v1/messages",
			Model: "claude-x", Route: "local", Status: 200, DurationMS: 12,
			PromptSHA256: digest(prompt), PromptBytes: len(prompt), Prompt: prompt, Answer: "answer " + secret,
		}
	}

	t.Run("off by default writes nothing", func(t *testing.T) {
		st, svc := setup(t)
		set, err := svc.PromptLogSettingOf(ctx, member, "pl-org-a")
		require.NoError(t, err)
		assert.Equal(t, set, accounts.PromptLogSetting{Enabled: false, RetentionDays: 30})
		stored, err := svc.RecordPromptLog(ctx, entry("pl-org-a", "hello"))
		require.NoError(t, err)
		assert.False(t, stored)
		rows, err := st.ListPromptLog(ctx, accounts.PromptLogFilter{OrgID: "pl-org-a"})
		require.NoError(t, err)
		assert.Len(t, rows, 0)
	})

	t.Run("only admins in a signed-in session change it", func(t *testing.T) {
		st, svc := setup(t)
		on := accounts.PromptLogSetting{Enabled: true, RetentionDays: 7}
		cases := []struct {
			name string
			p    accounts.Principal
			org  string
			set  accounts.PromptLogSetting
			want error
		}{
			{"member", member, "pl-org-a", on, accounts.ErrForbidden},
			{"api key", key, "pl-org-a", on, accounts.ErrForbidden},
			{"retention too short", owner, "pl-org-a", accounts.PromptLogSetting{Enabled: true, RetentionDays: 0}, accounts.ErrInvalidInput},
			{"retention too long", owner, "pl-org-a", accounts.PromptLogSetting{Enabled: true, RetentionDays: 401}, accounts.ErrInvalidInput},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				err := svc.SetPromptLog(ctx, c.p, c.org, c.set)
				assert.True(t, errors.Is(err, c.want), "got %v", err)
			})
		}
		err := svc.SetPromptLog(ctx, owner, "pl-org-b", on)
		assert.True(t, errors.Is(err, accounts.ErrForbidden) || errors.Is(err, accounts.ErrNotFound), "another organisation")
		require.NoError(t, svc.SetPromptLog(ctx, owner, "pl-org-a", on))
		set, err := st.PromptLogSetting(ctx, "pl-org-a")
		require.NoError(t, err)
		assert.Equal(t, set, on)
		set, err = st.PromptLogSetting(ctx, "pl-org-b")
		require.NoError(t, err)
		assert.False(t, set.Enabled, "the choice is per organisation")
		ev, err := st.ListEvents(ctx, accounts.EventFilter{OrgID: "pl-org-a", Action: accounts.ActionPromptLogSet, Limit: 10})
		require.NoError(t, err)
		require.Len(t, ev, 1, "the change is audited")
		assert.Equal(t, ev[0].Detail, "enabled=true retention_days=7")
	})

	t.Run("on writes redacted capped rows", func(t *testing.T) {
		st, svc := setup(t)
		require.NoError(t, svc.SetPromptLog(ctx, owner, "pl-org-a", accounts.PromptLogSetting{Enabled: true, RetentionDays: 30}))
		long := "key " + secret + " " + strings.Repeat("é", accounts.MaxPromptLogText)
		e := entry("pl-org-a", long)
		e.Shortened, e.Forwarded, e.ForwardedSHA256, e.ForwardedBytes = true, "short "+secret, digest("short"), 5
		stored, err := svc.RecordPromptLog(ctx, e)
		require.NoError(t, err)
		assert.True(t, stored)
		cloud := entry("pl-org-a", "plain")
		cloud.Route, cloud.Model, cloud.Fallthrough, cloud.PromptSHA256 = "cloud", "bad model!", "BAD", "not-a-digest"
		cloud.Forwarded = "ignored when not shortened"
		_, err = svc.RecordPromptLog(ctx, cloud)
		require.NoError(t, err)

		rows, err := st.ListPromptLog(ctx, accounts.PromptLogFilter{OrgID: "pl-org-a", Ascending: true})
		require.NoError(t, err)
		require.Len(t, rows, 2)
		r := rows[0]
		assert.NotContains(t, r.Prompt, secret)
		assert.NotContains(t, r.Forwarded, secret)
		assert.NotContains(t, r.Answer, secret)
		assert.LessOrEqual(t, len(r.Prompt), accounts.MaxPromptLogText)
		assert.True(t, strings.ToValidUTF8(r.Prompt, "") == r.Prompt, "cut on a character boundary")
		assert.Equal(t, r.PromptSHA256, digest(long), "the digest is of the text sent, so it matches the chain")
		assert.Equal(t, r.PromptBytes, len(long))
		assert.True(t, r.Shortened)
		assert.Equal(t, r.AuditID, int64(7))
		assert.Equal(t, r.At, now)
		c := rows[1]
		assert.Equal(t, c.Answer, "", "only the local model's answers are kept")
		assert.Equal(t, c.Model, "other")
		assert.Equal(t, c.Fallthrough, "other")
		assert.Equal(t, c.PromptSHA256, "")
		assert.Equal(t, c.Forwarded, "", "nothing forwarded when not shortened")
		assert.True(t, c.ID > r.ID)
	})

	t.Run("reads are admin only, audited and per organisation", func(t *testing.T) {
		st, svc := setup(t)
		for _, o := range []struct {
			p   accounts.Principal
			org string
		}{{owner, "pl-org-a"}, {other, "pl-org-b"}} {
			require.NoError(t, svc.SetPromptLog(ctx, o.p, o.org, accounts.PromptLogSetting{Enabled: true, RetentionDays: 30}))
		}
		for i, org := range []string{"pl-org-a", "pl-org-a", "pl-org-b"} {
			e := entry(org, "p")
			e.SessionID = []string{"s1", "s2", "s1"}[i]
			_, err := svc.RecordPromptLog(ctx, e)
			require.NoError(t, err)
		}
		cases := []struct {
			name string
			p    accounts.Principal
			org  string
			want error
		}{
			{"member", member, "pl-org-a", accounts.ErrForbidden},
			{"api key", key, "pl-org-a", accounts.ErrForbidden},
			{"other organisation", owner, "pl-org-b", accounts.ErrForbidden},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				_, err := svc.PromptLog(ctx, c.p, c.org, accounts.ActionPromptLogRead, accounts.PromptLogFilter{})
				assert.True(t, errors.Is(err, c.want) || errors.Is(err, accounts.ErrNotFound), "got %v", err)
			})
		}
		_, err := svc.PromptLog(ctx, owner, "pl-org-a", "other.action", accounts.PromptLogFilter{})
		assert.True(t, errors.Is(err, accounts.ErrInvalidInput))
		_, err = svc.PromptLog(ctx, owner, "pl-org-a", accounts.ActionPromptLogRead, accounts.PromptLogFilter{Route: "elsewhere"})
		assert.True(t, errors.Is(err, accounts.ErrInvalidInput))

		rows, err := svc.PromptLog(ctx, owner, "pl-org-a", accounts.ActionPromptLogRead, accounts.PromptLogFilter{OrgID: "pl-org-b"})
		require.NoError(t, err)
		assert.Len(t, rows, 2, "the organisation comes from the caller's choice, never the filter")
		rows, err = svc.PromptLog(ctx, owner, "pl-org-a", accounts.ActionPromptLogRead, accounts.PromptLogFilter{SessionID: "s2", Limit: 1})
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, rows[0].SessionID, "s2")
		older, err := svc.PromptLog(ctx, owner, "pl-org-a", accounts.ActionPromptLogRead, accounts.PromptLogFilter{BeforeID: rows[0].ID})
		require.NoError(t, err)
		require.Len(t, older, 1)
		assert.Equal(t, older[0].SessionID, "s1")

		ev, err := st.ListEvents(ctx, accounts.EventFilter{OrgID: "pl-org-a", Action: accounts.ActionPromptLogRead, Limit: 10})
		require.NoError(t, err)
		assert.Len(t, ev, 3, "every successful view is audited")
		assert.Equal(t, ev[0].Detail, "", "a filter-free view")
	})

	t.Run("retention prune and seal", func(t *testing.T) {
		st, svc := setup(t)
		require.NoError(t, svc.SetPromptLog(ctx, owner, "pl-org-a", accounts.PromptLogSetting{Enabled: true, RetentionDays: 2}))
		require.NoError(t, svc.SetPromptLog(ctx, other, "pl-org-b", accounts.PromptLogSetting{Enabled: true, RetentionDays: 30}))
		for _, d := range []int{5, 4, 1} {
			for _, org := range []string{"pl-org-a", "pl-org-b"} {
				e := entry(org, "p")
				e.At = now.Add(-time.Duration(d) * 24 * time.Hour)
				_, err := svc.RecordPromptLog(ctx, e)
				require.NoError(t, err)
			}
		}
		orgs, err := st.PromptLogOrgs(ctx)
		require.NoError(t, err)
		assert.Equal(t, orgs["pl-org-a"], 2)
		assert.Equal(t, orgs["pl-org-b"], 30)

		old, err := st.ListPromptLog(ctx, accounts.PromptLogFilter{OrgID: "pl-org-a", Until: now.Add(-2 * 24 * time.Hour), Ascending: true})
		require.NoError(t, err)
		require.Len(t, old, 2)
		seg := accounts.PromptLogSegment{OrgID: "pl-org-a", FirstID: old[0].ID, LastID: old[1].ID, FirstAt: old[0].At, LastAt: old[1].At,
			Count: 3, SHA256: digest("file"), Bytes: 10, Backend: "dir", Key: "k", SealedAt: now}
		_, err = st.SealPromptLogSegment(ctx, seg)
		assert.True(t, errors.Is(err, accounts.ErrAuditConflict), "a count that does not match changes nothing")
		seg.Count = 2
		seg, err = st.SealPromptLogSegment(ctx, seg)
		require.NoError(t, err)
		assert.True(t, seg.ID > 0)
		rows, err := st.ListPromptLog(ctx, accounts.PromptLogFilter{OrgID: "pl-org-a"})
		require.NoError(t, err)
		assert.Len(t, rows, 1, "sealed rows leave the database")
		rows, err = st.ListPromptLog(ctx, accounts.PromptLogFilter{OrgID: "pl-org-b"})
		require.NoError(t, err)
		assert.Len(t, rows, 3, "another organisation's rows stay")

		segs, err := st.PromptLogSegments(ctx, now)
		require.NoError(t, err)
		require.Len(t, segs, 1)
		assert.Equal(t, segs[0].SHA256, digest("file"))
		require.NoError(t, st.DropPromptLogSegment(ctx, segs[0].ID))
		segs, err = st.PromptLogSegments(ctx, now)
		require.NoError(t, err)
		assert.Len(t, segs, 0)

		n, err := st.PrunePromptLog(ctx, "pl-org-b", now.Add(-2*24*time.Hour))
		require.NoError(t, err)
		assert.Equal(t, n, 2)
		rows, err = st.ListPromptLog(ctx, accounts.PromptLogFilter{OrgID: "pl-org-b"})
		require.NoError(t, err)
		assert.Len(t, rows, 1)
	})
}
