package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/history"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// statsServer starts an API with accounts and a platform admin, and answers
// a signup helper for organisations.
func statsServer(t *testing.T) (*Server, accounts.Store, *memory.Store, *client, func(email string) (*client, accounts.User, accounts.Org)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	vm := httptest.NewServer(&fakeVM{})
	t.Cleanup(vm.Close)
	accSt := acctmem.New()
	acc := accounts.NewService(accSt, false)
	bootstrapAccounts(t, acc)
	acc.SignupOpen = true
	st := memory.New()
	srv := New(ctx, Config{Accounts: acc, ServiceToken: "svc-token", Metrics: &telemetry.Querier{URL: vm.URL}}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	signup := func(email string) (*client, accounts.User, accounts.Org) {
		c := newClient(t, ts)
		var signed struct {
			User accounts.User
			Org  accounts.Org
		}
		require.Equal(t, c.signUp(`{"email":"`+email+`","name":"N","password":"`+pw+`"}`, &signed), 202)
		return c, signed.User, signed.Org
	}
	ada, adaUser, adaOrg := signup("ada@example.com")
	require.NoError(t, acc.SetPlatformAdmin(ctx, rootSession(t, acc), adaUser.ID, true))
	require.NoError(t, accSt.RemoveMember(ctx, adaOrg.ID, adaUser.ID))
	return srv, accSt, st, ada, signup
}

func TestSecuritySection(t *testing.T) {
	ctx := context.Background()
	srv, accSt, st, ada, signup := statsServer(t)
	member, ua, orgA := signup("a@example.com")
	_, ub, orgB := signup("b@example.com")
	_, uc, orgC := signup("c@example.com")
	_, ud, orgD := signup("d@example.com")
	now := time.Now().UTC()
	attach := func(org accounts.Org, user accounts.User, graph string, access string) {
		require.NoError(t, accSt.AddRepository(ctx, accounts.Repository{OrgID: org.ID, Graph: graph, Remote: "https://example.com/" + graph + ".git",
			Visibility: accounts.Private, AddedAt: now, AddedBy: user.ID, Access: access}))
	}
	// shared-secret is held by A and B, lonely-secret by C; D's graph has
	// no snapshot yet and A's lost entry is not its posture any more.
	attach(orgA, ua, "acme/shared-secret", "")
	attach(orgB, ub, "acme/shared-secret", "")
	attach(orgC, uc, "acme/lonely-secret", "")
	attach(orgD, ud, "acme/unindexed-secret", "")
	attach(orgA, ua, "acme/lost-secret", accounts.AccessLost)
	snap := func(graph, at, grade string, findings map[string]int, rules map[string]int) {
		require.NoError(t, st.AddSnapshot(ctx, graph, history.Snapshot{At: at, Branch: "main", Grade: grade, Findings: findings,
			Vulns: map[string]int{"high": 1}, VulnsCalled: 1, Rules: rules}))
	}
	snap("acme/shared-secret", now.Add(-48*time.Hour).Format(time.RFC3339), "A", nil, nil)
	snap("acme/shared-secret", now.Add(-30*time.Minute).Format(time.RFC3339), "C", map[string]int{"high": 3},
		map[string]int{"code-injection": 2, "cypher-concat": 1})
	snap("acme/lonely-secret", now.Add(-time.Hour).Format(time.RFC3339), "B", map[string]int{"critical": 1}, map[string]int{"code-injection": 1})
	snap("acme/lost-secret", now.Add(-time.Hour).Format(time.RFC3339), "F", map[string]int{"critical": 9}, map[string]int{"debug-enabled": 9})
	require.NoError(t, accSt.PutFindingTriage(ctx, accounts.FindingTriage{OrgID: orgA.ID, Graph: "acme/shared-secret", Fingerprint: strings.Repeat("f", 64),
		Rule: "code-injection", File: "internal/db/query-secret.go", Reason: "reason-secret", State: accounts.TriageAcceptedRisk, ActorID: ua.ID, CreatedAt: now.Add(-time.Hour)}))

	t.Run("platform", func(t *testing.T) {
		var body string
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/security?range=24h", "", &body), 200)
		for _, secret := range []string{"shared-secret", "lonely-secret", "lost-secret", "unindexed-secret", "example.com", "query-secret", "reason-secret", ua.ID} {
			assert.False(t, strings.Contains(body, secret), "leaks "+secret)
		}
		var s SecuritySection
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/security?range=24h", "", &s), 200)
		assert.Equal(t, s.Totals.Graphs, 3, "each graph once, the lost entry left out")
		assert.Equal(t, s.Totals.Grades["c"], 1)
		assert.Equal(t, s.Totals.Grades["b"], 1)
		assert.Equal(t, s.Totals.Grades["none"], 1)
		assert.Equal(t, s.Totals.Findings["high"], 3)
		assert.Equal(t, s.Totals.Findings["critical"], 1)
		assert.Equal(t, s.Totals.Vulns["high"], 2)
		assert.Equal(t, s.Totals.VulnsCalled, 2)
		assert.Equal(t, s.Totals.Worse, 1, "A to C inside the range")
		assert.Equal(t, s.Totals.Suppressed["total"], 1)
		assert.Equal(t, s.Rules, []PlatformRuleCount{{Name: "code-injection", Findings: 3}, {Name: "other", Findings: 1}},
			"cypher-concat is in two organisations, so other")
		rows := map[string]OrgSecurityRow{}
		for _, r := range s.Rows {
			rows[r.OrgID] = r
		}
		assert.Equal(t, rows[orgA.ID].Graphs, 1)
		assert.Equal(t, rows[orgB.ID].Findings["high"], 3, "an organisation counts each of its entries")
		assert.Equal(t, rows[orgA.ID].Suppressed["accepted_risk"], 1)
		assert.Equal(t, rows[orgD.ID].Grades["none"], 1)

		var old SecuritySection
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/security?range=1h", "", &old), 200)
		assert.Equal(t, old.Totals.Worse, 1, "the grade dropped within the hour")
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/security?range=30d&sort=critical", "", &old), 200)
		assert.Equal(t, old.Rows[0].OrgID, orgC.ID)
	})

	t.Run("one organisation", func(t *testing.T) {
		var s SecuritySection
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/security?range=24h&org="+orgB.ID, "", &s), 200)
		assert.Equal(t, s.Totals.Graphs, 1)
		assert.True(t, s.Rules == nil, "no rule breakdown for one organisation")
		require.Len(t, s.Rows, 1)
		assert.Equal(t, member.do("GET", "/api/v1/admin/stats/security?range=24h", "", nil), 403)
	})

	t.Run("export", func(t *testing.T) {
		var csv string
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/export?section=security&range=24h&format=csv", "", &csv), 200)
		assert.True(t, strings.HasPrefix(csv, "slug,name,graphs,grade_a"), csv)
		assert.False(t, strings.Contains(csv, "acme") || strings.Contains(csv, "query-secret"), csv)
		var js string
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/export?section=security&range=24h&format=json", "", &js), 200)
		assert.Contains(t, js, `"rules":[{"name":"code-injection","findings":3}`)
	})

	t.Run("rollup", func(t *testing.T) {
		day := accounts.StatDay(now)
		set := accounts.NewPlatformStatSet(day)
		require.NoError(t, srv.securityStatSource(ctx, day, set))
		got := map[string]int64{}
		for _, r := range set.Rows() {
			if r.Value != 0 && strings.HasPrefix(r.Metric, "security_") {
				got[r.OrgID+"/"+r.Metric+"/"+r.Dimension] = r.Value
			}
		}
		for _, r := range set.Rows() {
			assert.False(t, strings.Contains(r.Dimension, "acme"), r.Dimension)
		}
		assert.Equal(t, got["/"+accounts.StatSecurityGrades+"/c"], int64(1), "the platform counts the shared graph once")
		assert.Equal(t, got["/"+accounts.StatSecurityFindings+"/high"], int64(3))
		assert.Equal(t, got[orgA.ID+"/"+accounts.StatSecurityFindings+"/high"], int64(3))
		assert.Equal(t, got[orgB.ID+"/"+accounts.StatSecurityFindings+"/high"], int64(3))
		assert.Equal(t, got["/"+accounts.StatSecurityRules+"/code-injection"], int64(3))
		assert.Equal(t, got["/"+accounts.StatSecurityWorse+"/graphs"], int64(1))
		assert.Equal(t, got["/"+accounts.StatSecuritySuppressed+"/total"], int64(1))
		assert.Equal(t, got[orgA.ID+"/"+accounts.StatSecuritySuppressed+"/accepted_risk"], int64(1))
		_, lost := got["/"+accounts.StatSecurityRules+"/debug-enabled"]
		assert.False(t, lost, "lost entries are left out")
	})
}

func TestAuditSection(t *testing.T) {
	ctx := context.Background()
	srv, accSt, _, ada, signup := statsServer(t)
	member, _, _ := signup("a@example.com")
	bad := newClient(t, httptest.NewServer(srv))
	assert.Equal(t, bad.do("POST", "/api/v1/auth/login", `{"email":"a@example.com","password":"wrong-password-1"}`, nil), 401)

	// The api-loops lease runs the first verification when the server starts.
	deadline := time.Now().Add(10 * time.Second)
	var first []accounts.AuditVerification
	for time.Now().Before(deadline) {
		var err error
		first, err = accSt.AuditVerifications(ctx, 10)
		require.NoError(t, err)
		if len(first) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.Len(t, first, 1)
	assert.Equal(t, first[0].Trigger, accounts.VerifyScheduled)
	assert.True(t, first[0].OK)

	t.Run("scheduled verification", func(t *testing.T) {
		wait := srv.verifyAuditChainOnce(ctx, first[0].At.Add(time.Hour))
		assert.Equal(t, wait, 23*time.Hour, "not due: wait for the day to pass")
		vs, err := accSt.AuditVerifications(ctx, 10)
		require.NoError(t, err)
		assert.Len(t, vs, 1)

		wait = srv.verifyAuditChainOnce(ctx, first[0].At.Add(25*time.Hour))
		vs, err = accSt.AuditVerifications(ctx, 10)
		require.NoError(t, err)
		assert.Len(t, vs, 2, "a day old: verified again")
		assert.Equal(t, wait, auditVerifyRetry, "never sooner than the retry")
	})

	t.Run("views read the stored result", func(t *testing.T) {
		// A planted broken result while the chain itself is fine: views
		// show it, which they would not if they walked the chain.
		planted, err := accSt.AddAuditVerification(ctx, accounts.AuditVerification{At: time.Now().UTC(), Trigger: accounts.VerifyScheduled,
			Verification: accounts.Verification{OK: false, Checked: 3, BrokenAt: 42, Reason: "planted"}})
		require.NoError(t, err)
		before, err := accSt.AuditVerifications(ctx, 100)
		require.NoError(t, err)

		var a AuditSection
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/audit?range=24h", "", &a), 200)
		require.True(t, a.Chain != nil)
		assert.Equal(t, a.Chain.ID, planted.ID)
		assert.False(t, a.Chain.OK)
		assert.Equal(t, a.Chain.BrokenAt, int64(42))
		assert.Equal(t, len(a.Verifications), len(before))
		var sum StatsSummary
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/summary?range=24h", "", &sum), 200)
		require.True(t, sum.Chain != nil)
		assert.Equal(t, sum.Chain.ID, planted.ID)
		after, err := accSt.AuditVerifications(ctx, 100)
		require.NoError(t, err)
		assert.Equal(t, len(after), len(before), "a view never runs a verification")

		// The on-demand check walks the chain and stores its result.
		var v accounts.Verification
		require.Equal(t, ada.do("GET", "/api/v1/admin/audit/verify", "", &v), 200)
		assert.True(t, v.OK)
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/audit?range=24h", "", &a), 200)
		assert.True(t, a.Chain.OK)
		assert.Equal(t, a.Chain.Trigger, accounts.VerifyOnDemand)
	})

	t.Run("counts", func(t *testing.T) {
		var a AuditSection
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/audit?range=24h", "", &a), 200)
		assert.Equal(t, a.Step, "hour")
		assert.True(t, a.Rollup == nil)
		assert.True(t, len(a.Buckets) > 0)
		failed := 0
		for _, l := range a.Logins {
			failed += l.N
		}
		assert.Equal(t, failed, 1, "the wrong password")
		var body string
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/audit?range=24h", "", &body), 200)
		for _, secret := range []string{"a@example.com", "wrong-password", "/api/v1/"} {
			assert.False(t, strings.Contains(body, secret), "leaks "+secret)
		}

		var long AuditSection
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/audit?range=90d", "", &long), 200)
		assert.Equal(t, long.Step, "day")
		require.True(t, long.Rollup != nil)
		assert.Equal(t, long.Rollup.Days, 90)
		failed = 0
		for _, l := range long.Logins {
			failed += l.N
		}
		assert.Equal(t, failed, 1, "today is counted live")

		assert.Equal(t, ada.do("GET", "/api/v1/admin/stats/audit?range=24h&org=0123456789abcdef01234567", "", nil), 400, "platform-wide only")
		assert.Equal(t, member.do("GET", "/api/v1/admin/stats/audit?range=24h", "", nil), 403)
		var csv string
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/export?section=audit&range=24h&format=csv", "", &csv), 200)
		assert.True(t, strings.HasPrefix(csv, "kind,name,success,failure"), csv)
		assert.Contains(t, csv, "chain,verified")
	})

	t.Run("rollup", func(t *testing.T) {
		day := accounts.StatDay(time.Now())
		set := accounts.NewPlatformStatSet(day)
		require.NoError(t, srv.auditStatSource(ctx, day, set))
		got := map[string]int64{}
		for _, r := range set.Rows() {
			require.True(t, accounts.ValidPlatformStat(r.Metric, r.Dimension, r.OrgID), r.Metric+"/"+r.Dimension)
			assert.Equal(t, r.OrgID, "", "audit figures are platform only")
			got[r.Metric+"/"+r.Dimension] = r.Value
		}
		assert.Equal(t, got[accounts.StatAuditLogins+"/wrong_password"], int64(1))
		assert.Equal(t, got[accounts.StatAuditChain+"/broken"], int64(1), "the planted result")
		assert.True(t, got[accounts.StatAuditChain+"/verified"] >= 3)
	})
}
