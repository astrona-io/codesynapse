package accounts

import (
	"context"
	"slices"
	"strings"
	"time"
)

// Audit statistics (docs/PLATFORM_STATS_PLAN.md, phase 7): counts of the
// audit log by action family and outcome, failed sign-ins by cause and the
// security events, and the stored results of chain verification. Counts
// only: no actor, target, address or detail text leaves this file, except
// the fixed cause names below.

// AuditFamilies are the action families the statistics name; any other
// family counts as "other". An HTTP entry's action is "http <pattern>", so
// its family is "http".
var AuditFamilies = []string{
	"admin", "agent", "audit", "auth", "execution_session", "finding", "gateway", "git_token", "http", "identity",
	"ingest", "job", "key", "mcp", "member", "org", "repo", "root", "setup", "sso", "team", "user", "user_git_token", "watch",
}

// AuditFamilyOther is the family of an action outside AuditFamilies.
const AuditFamilyOther = "other"

// AuditFamily is the family of an action, folded into AuditFamilies.
func AuditFamily(action string) string {
	f := action
	if i := strings.IndexAny(action, ". "); i >= 0 {
		f = action[:i]
	}
	if slices.Contains(AuditFamilies, f) {
		return f
	}
	return AuditFamilyOther
}

// LoginCauses are the causes of a failed password sign-in, by the constant
// detail Login writes; any other detail (or an SSO failure) is "other".
var LoginCauses = map[string]string{
	"wrong password":    "wrong_password",
	"unknown login":     "unknown_login",
	"rate limited":      "rate_limited",
	"account suspended": "suspended",
}

// LoginCauseNames lists the causes in display order, "other" last.
var LoginCauseNames = []string{"wrong_password", "unknown_login", "rate_limited", "suspended", "other"}

// LoginCause names the cause of a failed sign-in from its detail.
func LoginCause(detail string) string {
	if c, ok := LoginCauses[detail]; ok {
		return c
	}
	return "other"
}

// AuditSecurityActions are the security events counted on their own, by
// their statistics name.
var AuditSecurityActions = map[string]string{
	"user.suspend":               "suspend",
	"user.resume":                "resume",
	"user.platform_admin.grant":  "platform_admin_grant",
	"user.platform_admin.revoke": "platform_admin_revoke",
	"root.denied":                "root_denied",
	"auth.denied":                "auth_denied",
}

// AuditSecurityNames lists the security event names in display order.
var AuditSecurityNames = []string{"suspend", "resume", "platform_admin_grant", "platform_admin_revoke", "root_denied", "auth_denied"}

// AuditSecurityActionList is AuditSecurityActions' keys, sorted: the
// actions AuditStats counts as security events.
func AuditSecurityActionList() []string {
	out := make([]string, 0, len(AuditSecurityActions))
	for a := range AuditSecurityActions {
		out = append(out, a)
	}
	slices.Sort(out)
	return out
}

// AuditHourCount is the number of entries of one family and outcome in one
// UTC hour. Family is the action's raw prefix as the store grouped it;
// callers fold it with AuditFamily.
type AuditHourCount struct {
	Hour    time.Time `json:"hour"`
	Family  string    `json:"family"`
	Outcome string    `json:"outcome"`
	N       int       `json:"n"`
}

// AuditNamedCount is a count by a fixed name.
type AuditNamedCount struct {
	Name string `json:"name"`
	N    int    `json:"n"`
}

// AuditCounts are the audit log's counts in a window [Since, Until).
// Logins are the user.login failures by their detail and Security the
// AuditSecurityActions by action, both as the store grouped them.
type AuditCounts struct {
	Hours    []AuditHourCount
	Logins   []AuditNamedCount
	Security []AuditNamedCount
}

// MaxAuditCountRows bounds the hour rows one AuditStats call returns.
const MaxAuditCountRows = 50_000

// Verification triggers.
const (
	VerifyScheduled = "scheduled"
	VerifyOnDemand  = "on_demand"
)

// MaxAuditVerifications bounds one read of the stored results.
const MaxAuditVerifications = 500

// AuditVerificationRetention is how long results are kept.
const AuditVerificationRetention = DefaultRetention

// AuditVerification is one stored run of the chain verification: when, how
// it was started, how long it took and its result. RunError means the run
// could not finish (a store error), which is not a broken chain.
type AuditVerification struct {
	ID         int64     `json:"id"`
	At         time.Time `json:"at"`
	Trigger    string    `json:"trigger"`
	Deep       bool      `json:"deep"`
	DurationMS int64     `json:"duration_ms"`
	RunError   bool      `json:"run_error"`
	Verification
}

// verifyRunError is the reason stored for a run that could not finish; the
// error itself goes to the log, never into the stored result.
const verifyRunError = "the verification could not finish"

// AuditStatsStore is the part of Store the audit statistics use.
type AuditStatsStore interface {
	// AuditStats counts the entries in [since, until): per UTC hour, raw
	// action family and outcome; user.login failures by detail; and the
	// actions of AuditSecurityActionList. Bounded by MaxAuditCountRows.
	AuditStats(ctx context.Context, since, until time.Time) (AuditCounts, error)
	// AddAuditVerification stores a result and deletes results older than
	// AuditVerificationRetention before its time.
	AddAuditVerification(ctx context.Context, v AuditVerification) (AuditVerification, error)
	// AuditVerifications lists stored results, newest first.
	AuditVerifications(ctx context.Context, limit int) ([]AuditVerification, error)
	// TriageCounts counts the active finding triage decisions at now per
	// organisation and state.
	TriageCounts(ctx context.Context, now time.Time) (map[string]map[string]int, error)
}

// PrepareAuditVerification validates and cleans a result before a store
// writes it.
func PrepareAuditVerification(v AuditVerification) (AuditVerification, error) {
	if v.At.IsZero() || (v.Trigger != VerifyScheduled && v.Trigger != VerifyOnDemand) || v.Checked < 0 || v.DurationMS < 0 {
		return AuditVerification{}, ErrInvalidInput
	}
	v.At = v.At.UTC().Truncate(time.Microsecond)
	if r := []rune(v.Reason); len(r) > 200 {
		v.Reason = string(r[:200])
	}
	return v, nil
}

// RunAuditVerification verifies the chain with verify and stores the
// result, whether the chain verified, broke or the run failed. The stored
// result is what the statistics show; err is the run's own error.
func (s *Service) RunAuditVerification(ctx context.Context, trigger string, deep bool, verify func(context.Context) (Verification, error)) (AuditVerification, error) {
	start := time.Now()
	at := s.now()
	v, err := verify(ctx)
	rec := AuditVerification{At: at, Trigger: trigger, Deep: deep, DurationMS: time.Since(start).Milliseconds(), Verification: v}
	if err != nil {
		rec.Verification = Verification{Reason: verifyRunError}
		rec.RunError = true
	}
	stored, serr := s.St.AddAuditVerification(ctx, rec)
	if err != nil {
		return rec, err
	}
	return stored, serr
}

// LatestAuditVerification is the newest stored result, or nil.
func (s *Service) LatestAuditVerification(ctx context.Context) (*AuditVerification, error) {
	got, err := s.St.AuditVerifications(ctx, 1)
	if err != nil || len(got) == 0 {
		return nil, err
	}
	return &got[0], nil
}
