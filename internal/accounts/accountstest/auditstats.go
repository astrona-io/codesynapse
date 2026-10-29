package accountstest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// RunAuditStats covers the audit and security statistics: counts by hour,
// family and outcome in a window, failed sign-ins by detail, the security
// actions, stored chain verifications (order, retention) and the active
// triage decisions per organisation.
func RunAuditStats(t *testing.T, newStore func(*testing.T) accounts.Store) {
	ctx := context.Background()
	base := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)

	t.Run("counts", func(t *testing.T) {
		st := newStore(t)
		add := func(at time.Time, action, outcome, detail string) {
			_, err := st.AddEvent(ctx, accounts.Event{At: at, Action: action, Outcome: outcome, Detail: detail, Via: "service"})
			require.NoError(t, err)
		}
		add(base.Add(-time.Minute), "user.login", accounts.Failure, "wrong password") // before the window
		add(base.Add(5*time.Minute), "user.login", accounts.Failure, "wrong password")
		add(base.Add(6*time.Minute), "user.login", accounts.Failure, "wrong password")
		add(base.Add(7*time.Minute), "user.login", accounts.Failure, "unknown login")
		add(base.Add(8*time.Minute), "user.login", accounts.Success, "")
		add(base.Add(70*time.Minute), "http GET /api/v1/repos", accounts.Success, "")
		add(base.Add(71*time.Minute), "user.suspend", accounts.Success, "")
		add(base.Add(72*time.Minute), "root.denied", accounts.Failure, "")
		add(base.Add(2*time.Hour), "user.suspend", accounts.Success, "") // at until: outside

		c, err := st.AuditStats(ctx, base, base.Add(2*time.Hour))
		require.NoError(t, err)
		h1, h2 := base, base.Add(time.Hour)
		assert.Equal(t, c.Hours, []accounts.AuditHourCount{
			{Hour: h1, Family: "user", Outcome: accounts.Failure, N: 3},
			{Hour: h1, Family: "user", Outcome: accounts.Success, N: 1},
			{Hour: h2, Family: "http", Outcome: accounts.Success, N: 1},
			{Hour: h2, Family: "root", Outcome: accounts.Failure, N: 1},
			{Hour: h2, Family: "user", Outcome: accounts.Success, N: 1},
		})
		assert.Equal(t, c.Logins, []accounts.AuditNamedCount{{Name: "unknown login", N: 1}, {Name: "wrong password", N: 2}})
		assert.Equal(t, c.Security, []accounts.AuditNamedCount{{Name: "root.denied", N: 1}, {Name: "user.suspend", N: 1}})

		empty, err := st.AuditStats(ctx, base.Add(24*time.Hour), base.Add(25*time.Hour))
		require.NoError(t, err)
		assert.Len(t, empty.Hours, 0)
		assert.Len(t, empty.Logins, 0)
		assert.Len(t, empty.Security, 0)
	})

	t.Run("verifications", func(t *testing.T) {
		st := newStore(t)
		_, err := st.AddAuditVerification(ctx, accounts.AuditVerification{At: base, Trigger: "cron"})
		assert.ErrorIs(t, err, accounts.ErrInvalidInput)

		old, err := st.AddAuditVerification(ctx, accounts.AuditVerification{At: base.AddDate(0, 0, -500), Trigger: accounts.VerifyScheduled,
			Verification: accounts.Verification{OK: true, Checked: 3}})
		require.NoError(t, err)
		assert.True(t, old.ID > 0)
		a, err := st.AddAuditVerification(ctx, accounts.AuditVerification{At: base, Trigger: accounts.VerifyScheduled, DurationMS: 12,
			Verification: accounts.Verification{OK: true, Checked: 40, FirstID: 2, Segments: 1, Sealed: 10}})
		require.NoError(t, err)
		b, err := st.AddAuditVerification(ctx, accounts.AuditVerification{At: base.Add(time.Hour), Trigger: accounts.VerifyOnDemand, Deep: true,
			Verification: accounts.Verification{OK: false, Checked: 7, BrokenAt: 9, Reason: strings.Repeat("r", 300), Archives: 1}})
		require.NoError(t, err)

		got, err := st.AuditVerifications(ctx, 10)
		require.NoError(t, err)
		require.Len(t, got, 2) // the 500-day-old one was pruned
		assert.Equal(t, got[0].ID, b.ID)
		assert.Equal(t, got[0].Trigger, accounts.VerifyOnDemand)
		assert.True(t, got[0].Deep)
		assert.False(t, got[0].OK)
		assert.Equal(t, got[0].BrokenAt, int64(9))
		assert.Equal(t, len(got[0].Reason), 200)
		assert.Equal(t, got[0].Archives, 1)
		assert.Equal(t, got[1].ID, a.ID)
		assert.Equal(t, got[1].At, base)
		assert.Equal(t, got[1].Verification, accounts.Verification{OK: true, Checked: 40, FirstID: 2, Segments: 1, Sealed: 10})
		assert.Equal(t, got[1].DurationMS, int64(12))

		one, err := st.AuditVerifications(ctx, 1)
		require.NoError(t, err)
		require.Len(t, one, 1)
		assert.Equal(t, one[0].ID, b.ID)
	})

	t.Run("triage counts", func(t *testing.T) {
		st := newStore(t)
		now := time.Now().UTC()
		require.NoError(t, st.CreateUser(ctx, accounts.User{ID: "as-owner", Name: "o", Email: "as-owner@example.com", CreatedAt: now}))
		for _, org := range []string{"as-org-a", "as-org-b"} {
			require.NoError(t, st.CreateOrg(ctx, accounts.Org{ID: org, Name: org, Slug: org, CreatedAt: now}, "as-owner"))
			require.NoError(t, st.AddRepository(ctx, accounts.Repository{OrgID: org, Graph: "as/repo", AddedBy: "as-owner", Visibility: accounts.Private, AddedAt: now}))
		}
		expired := now.Add(-time.Hour)
		for i, tr := range []accounts.FindingTriage{
			{OrgID: "as-org-a", State: accounts.TriageAcceptedRisk},
			{OrgID: "as-org-a", State: accounts.TriageAcceptedRisk},
			{OrgID: "as-org-a", State: accounts.TriageFalsePositive},
			{OrgID: "as-org-a", State: accounts.TriageAcceptedRisk, ExpiresAt: &expired},
			{OrgID: "as-org-b", State: accounts.TriageFixedBy, Reference: "#12"},
		} {
			tr.Graph, tr.Fingerprint, tr.Rule, tr.File, tr.Reason, tr.ActorID, tr.CreatedAt = "as/repo", strings.Repeat(string(rune('a'+i)), 64), "go-sql-injection", "db/q.go", "why", "as-owner", now.Add(-2*time.Hour)
			require.NoError(t, st.PutFindingTriage(ctx, tr))
		}
		got, err := st.TriageCounts(ctx, now)
		require.NoError(t, err)
		assert.Equal(t, got, map[string]map[string]int{
			"as-org-a": {accounts.TriageAcceptedRisk: 2, accounts.TriageFalsePositive: 1},
			"as-org-b": {accounts.TriageFixedBy: 1},
		})
	})
}
