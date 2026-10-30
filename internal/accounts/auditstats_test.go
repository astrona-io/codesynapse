package accounts

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestAuditFamily(t *testing.T) {
	for action, want := range map[string]string{
		"user.login":                 "user",
		"user.platform_admin.grant":  "user",
		"http GET /api/v1/repos":     "http",
		"mcp.tools/call":             "mcp",
		"gateway.request":            "gateway",
		"something.new":              AuditFamilyOther,
		"":                           AuditFamilyOther,
		"user_git_token.set":         "user_git_token",
		"execution_session.register": "execution_session",
	} {
		assert.Equal(t, AuditFamily(action), want, action)
	}
}

func TestLoginCause(t *testing.T) {
	assert.Equal(t, LoginCause("wrong password"), "wrong_password")
	assert.Equal(t, LoginCause("rate limited"), "rate_limited")
	assert.Equal(t, LoginCause("a@example.com tried"), "other")
	for _, c := range LoginCauses {
		assert.True(t, ValidPlatformStat(StatAuditLogins, c, ""), c)
	}
}

func TestSecurityGradeDim(t *testing.T) {
	assert.Equal(t, SecurityGradeDim("A", true), "a")
	assert.Equal(t, SecurityGradeDim("f", true), "f")
	assert.Equal(t, SecurityGradeDim("N/A", true), "na")
	assert.Equal(t, SecurityGradeDim("", true), "na")
	assert.Equal(t, SecurityGradeDim("A", false), "none")
}

// The rollup table refuses dimensions outside this pattern, so every
// security and audit dimension (catalogue rule IDs included) must match it.
func TestSecurityAuditDimensions(t *testing.T) {
	dim := regexp.MustCompile(`^[a-z0-9_.-]{1,40}$`)
	for _, m := range []string{StatSecurityGrades, StatSecurityFindings, StatSecurityVulns, StatSecurityWorse, StatSecurityRules,
		StatSecuritySuppressed, StatAuditEvents, StatAuditLogins, StatAuditSecurity, StatAuditChain} {
		dims := PlatformStatDimensions(m)
		assert.True(t, len(dims) > 0, m)
		for _, d := range dims {
			assert.True(t, dim.MatchString(d), m+"/"+d)
		}
	}
	assert.True(t, len(PlatformStatDimensions(StatSecurityRules)) > 10, "the catalogue's rules")
	assert.False(t, ValidPlatformStat(StatAuditEvents, "user.success", "org-1"), "audit figures are platform only")
	assert.True(t, ValidPlatformStat(StatSecurityRules, PlatformStatDimensions(StatSecurityRules)[0], "org-1"))
}

type verifyStore struct {
	Store
	added []AuditVerification
}

func (v *verifyStore) AddAuditVerification(_ context.Context, a AuditVerification) (AuditVerification, error) {
	a, err := PrepareAuditVerification(a)
	if err != nil {
		return a, err
	}
	a.ID = int64(len(v.added) + 1)
	v.added = append(v.added, a)
	return a, nil
}

func TestRunAuditVerification(t *testing.T) {
	ctx := context.Background()
	st := &verifyStore{}
	now := time.Date(2026, 10, 29, 1, 2, 3, 0, time.UTC)
	svc := &Service{St: st, Now: func() time.Time { return now }}

	got, err := svc.RunAuditVerification(ctx, VerifyScheduled, false, func(context.Context) (Verification, error) {
		return Verification{OK: false, Checked: 5, BrokenAt: 3, Reason: "hash mismatch"}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, got.ID, int64(1))
	assert.Equal(t, got.At, now)
	assert.False(t, got.OK)
	assert.Equal(t, got.BrokenAt, int64(3))

	boom := errors.New("connection reset by db.internal:5432")
	got, err = svc.RunAuditVerification(ctx, VerifyOnDemand, true, func(context.Context) (Verification, error) {
		return Verification{}, boom
	})
	assert.ErrorIs(t, err, boom)
	require.Len(t, st.added, 2)
	stored := st.added[1]
	assert.True(t, stored.RunError)
	assert.True(t, stored.Deep)
	assert.Equal(t, stored.Reason, verifyRunError, "the error text is not stored")
	assert.Equal(t, got.Trigger, VerifyOnDemand)
}
