package telemetry

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// counted keeps the tenant of ctx and drops its tool and model: the phase 4
// instruments are counted per tenant only, so an audit write made inside an
// MCP tool call does not open one series per tool.
func counted(ctx context.Context) context.Context {
	return WithModel(WithTool(ctx, "none"), "none", "none")
}

// Git operations: the fixed values of codesynapse_git_operations_total.
const (
	GitClone    = "clone"
	GitFetch    = "fetch"
	GitLsRemote = "ls_remote"
	GitPush     = "push"

	GitOK         = "ok"
	GitError      = "error"
	GitNoAccess   = "no_access"
	GitRedirected = "redirected"
	GitTimeout    = "timeout"
)

var gitOperations = map[string]bool{GitClone: true, GitFetch: true, GitLsRemote: true, GitPush: true}

var gitOutcomes = map[string]bool{GitOK: true, GitError: true, GitNoAccess: true, GitRedirected: true, GitTimeout: true}

// RecordGit counts one remote git call and its time. Values outside the
// fixed sets are dropped.
func RecordGit(ctx context.Context, operation, outcome string, d time.Duration) {
	if !gitOperations[operation] || !gitOutcomes[outcome] {
		return
	}
	ctx = counted(ctx)
	GitOperations.IncContext(ctx, operation, outcome)
	GitDuration.ObserveContext(ctx, max(0, d.Seconds()), operation)
}

// External lookup services and outcomes: the fixed values of
// codesynapse_lookups_total.
const (
	LookupOSV          = "osv"
	LookupDepsDev      = "depsdev"
	LookupForgeGitHub  = "forge_github"
	LookupForgeGitLab  = "forge_gitlab"
	LookupForgeForgejo = "forge_forgejo"

	LookupOK          = "ok"
	LookupError       = "error"
	LookupTimeout     = "timeout"
	LookupRateLimited = "rate_limited"
)

var lookupServices = map[string]bool{LookupOSV: true, LookupDepsDev: true, LookupForgeGitHub: true, LookupForgeGitLab: true, LookupForgeForgejo: true}

var lookupOutcomes = map[string]bool{LookupOK: true, LookupError: true, LookupTimeout: true, LookupRateLimited: true}

// LookupOutcome maps an HTTP answer (status, or 0 with err when there was
// none) to an outcome, from status codes and error types only, never from
// error text. A 404 is an answer (the package or organisation is not known),
// so it counts as ok. A cancelled request returns "", which is not recorded:
// the caller gave up, the service did not fail.
func LookupOutcome(ctx context.Context, status int, err error) string {
	if err != nil {
		var ne net.Error
		switch {
		case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
			return LookupTimeout
		case errors.Is(err, context.Canceled) || ctx.Err() != nil:
			return ""
		}
		return LookupError
	}
	switch {
	case status == http.StatusTooManyRequests:
		return LookupRateLimited
	case status >= 200 && status < 300, status == http.StatusNotFound:
		return LookupOK
	}
	return LookupError
}

// RecordHTTPLookup records one lookup from what http.Client.Do returned,
// timed from start. A 403 with X-RateLimit-Remaining: 0 is how GitHub and
// Forgejo say "rate limited", so it counts as rate_limited.
func RecordHTTPLookup(ctx context.Context, service string, start time.Time, res *http.Response, err error) {
	status := 0
	if res != nil {
		status = res.StatusCode
		if status == http.StatusForbidden && res.Header.Get("X-RateLimit-Remaining") == "0" {
			status = http.StatusTooManyRequests
		}
	}
	RecordLookup(ctx, service, LookupOutcome(ctx, status, err), time.Since(start))
}

// RecordLookup counts one external lookup and its time. Values outside the
// fixed sets (and the "" of a cancelled request) are dropped.
func RecordLookup(ctx context.Context, service, outcome string, d time.Duration) {
	if !lookupServices[service] || !lookupOutcomes[outcome] {
		return
	}
	ctx = counted(ctx)
	Lookups.IncContext(ctx, service, outcome)
	LookupDuration.ObserveContext(ctx, max(0, d.Seconds()), service)
}

// Sign-in methods and outcomes: the fixed values of
// codesynapse_auth_attempts_total.
const (
	AuthPassword = "password"
	AuthAPIKey   = "api_key" // every bearer credential, the installation token included
	AuthSession  = "session"
	AuthSSO      = "sso"

	AuthOK             = "ok"
	AuthBadCredentials = "bad_credentials"
	AuthRateLimited    = "rate_limited"
	AuthSuspended      = "suspended"
	AuthExpired        = "expired"
	AuthRefused        = "refused"
)

var authMethods = map[string]bool{AuthPassword: true, AuthAPIKey: true, AuthSession: true, AuthSSO: true}

var authOutcomes = map[string]bool{AuthOK: true, AuthBadCredentials: true, AuthRateLimited: true, AuthSuspended: true, AuthExpired: true, AuthRefused: true}

// RecordAuth counts one credential check. The tenant is the one ctx carries:
// none before authentication, so failures are platform series. Values
// outside the fixed sets are dropped.
func RecordAuth(ctx context.Context, method, outcome string) {
	if !authMethods[method] || !authOutcomes[outcome] {
		return
	}
	AuthAttempts.IncContext(counted(ctx), method, outcome)
}

// Audit log sources: the fixed values of codesynapse_audit_writes_total.
const (
	AuditAPI     = "api"
	AuditMCP     = "mcp"
	AuditService = "service" // background work outside any request
)

var auditSources = map[string]bool{AuditAPI: true, AuditMCP: true, AuditService: true}

type auditSourceKey struct{}

// WithAuditSource records which service's requests ctx belongs to; audit
// writes made under it count for that source. Unknown sources are ignored.
func WithAuditSource(ctx context.Context, source string) context.Context {
	if !auditSources[source] {
		return ctx
	}
	return context.WithValue(ctx, auditSourceKey{}, source)
}

// AuditSource is the source ctx was marked with, else "service".
func AuditSource(ctx context.Context) string {
	if s, ok := ctx.Value(auditSourceKey{}).(string); ok {
		return s
	}
	return AuditService
}

// RecordAuditWrite counts one audit log write and its time, for the source
// and tenant ctx carries.
func RecordAuditWrite(ctx context.Context, d time.Duration, ok bool) {
	outcome := "ok"
	if !ok {
		outcome = "error"
	}
	source := AuditSource(ctx)
	ctx = counted(ctx)
	AuditWrites.IncContext(ctx, source, outcome)
	AuditWriteDuration.ObserveContext(ctx, max(0, d.Seconds()), source)
}

// Audit sealing outcomes: the fixed values of
// platform_codesynapse_audit_seals_total.
const (
	SealOK        = "ok"
	SealError     = "error"
	SealNothing   = "nothing_to_seal"
	SealNoArchive = "no_archive" // sealed files exist but no archive is configured: pruning waits
)

// RecordAuditSeal counts one sealing run. Unknown outcomes are dropped.
func RecordAuditSeal(ctx context.Context, outcome string) {
	switch outcome {
	case SealOK, SealError, SealNothing, SealNoArchive:
		AuditSeals.IncContext(ctx, outcome)
	}
}
