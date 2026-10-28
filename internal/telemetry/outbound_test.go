package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestLookupOutcome(t *testing.T) {
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name   string
		ctx    context.Context
		status int
		err    error
		want   string
	}{
		{"ok", context.Background(), http.StatusOK, nil, LookupOK},
		{"not found is an answer", context.Background(), http.StatusNotFound, nil, LookupOK},
		{"rate limited", context.Background(), http.StatusTooManyRequests, nil, LookupRateLimited},
		{"server error", context.Background(), http.StatusBadGateway, nil, LookupError},
		{"refused", context.Background(), http.StatusUnauthorized, nil, LookupError},
		{"deadline", context.Background(), 0, fmt.Errorf("osv: %w", context.DeadlineExceeded), LookupTimeout},
		{"net timeout", context.Background(), 0, fmt.Errorf("osv: %w", timeoutErr{}), LookupTimeout},
		{"dial error", context.Background(), 0, errors.New("connection refused"), LookupError},
		{"cancelled", gone, 0, context.Canceled, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, LookupOutcome(c.ctx, c.status, c.err), c.want)
		})
	}
}

func TestOutboundMetricsStayInTheirSets(t *testing.T) {
	ctx := WithTenant(context.Background(), "dddddddddddddddddddddddd")
	// A tool and model on the context must not become series of these metrics.
	ctx = WithModel(WithTool(ctx, "search"), "openai", "gpt-x")

	RecordGit(ctx, GitClone, GitNoAccess, time.Second)
	RecordGit(ctx, "ls-remote", GitOK, time.Second)
	RecordGit(ctx, GitFetch, "github.com", time.Second)
	RecordLookup(ctx, LookupOSV, LookupRateLimited, 300*time.Millisecond)
	RecordLookup(ctx, "api.example.com", LookupOK, time.Second)
	RecordLookup(ctx, LookupDepsDev, "", time.Second)
	RecordAuth(ctx, AuthAPIKey, AuthOK)
	RecordAuth(ctx, "basic", AuthOK)
	RecordAuditWrite(WithAuditSource(ctx, AuditMCP), time.Millisecond, false)
	RecordAuditWrite(WithAuditSource(ctx, "web"), time.Millisecond, true)
	RecordAuditSeal(context.Background(), SealNoArchive)
	RecordAuditSeal(context.Background(), "maybe")
	// No trusted tenant: platform series.
	RecordAuth(context.Background(), AuthPassword, AuthBadCredentials)
	RecordGit(WithTenant(context.Background(), "not-a-tenant"), GitPush, GitTimeout, time.Second)

	out := text(t, Default)
	const id = `tenant_id="dddddddddddddddddddddddd"`
	const none = `mcp_tool="none",llm_provider="none",llm_model="none"`
	assert.Contains(t, out, `codesynapse_git_operations_total{operation="clone",outcome="no_access",`+id+","+none)
	assert.Contains(t, out, `codesynapse_git_operation_seconds_bucket{operation="clone",`+id)
	assert.False(t, strings.Contains(out, `operation="ls-remote"`), "operations come from a fixed set")
	assert.False(t, strings.Contains(out, "github.com"), "outcomes come from a fixed set")
	assert.Contains(t, out, `codesynapse_lookups_total{service="osv",outcome="rate_limited",`+id+","+none)
	assert.Contains(t, out, `codesynapse_lookup_seconds_count{service="osv",`+id)
	assert.False(t, strings.Contains(out, "example.com"), "services come from a fixed set")
	assert.False(t, strings.Contains(out, `service="depsdev"`), "a cancelled lookup is not recorded")
	assert.Contains(t, out, `codesynapse_auth_attempts_total{method="api_key",outcome="ok",`+id+","+none)
	assert.False(t, strings.Contains(out, `method="basic"`))
	assert.Contains(t, out, `codesynapse_audit_writes_total{source="mcp",outcome="error",`+id)
	assert.Contains(t, out, `codesynapse_audit_writes_total{source="service",outcome="ok",`+id)
	assert.Contains(t, out, `codesynapse_audit_write_seconds_count{source="mcp",`+id)
	assert.False(t, strings.Contains(out, `source="web"`))
	assert.Contains(t, out, `platform_codesynapse_audit_seals_total{outcome="no_archive"}`)
	assert.False(t, strings.Contains(out, "maybe"))
	assert.Contains(t, out, `platform_codesynapse_auth_attempts_total{method="password",outcome="bad_credentials"`)
	assert.Contains(t, out, `platform_codesynapse_git_operations_total{operation="push",outcome="timeout"`)
	assert.False(t, strings.Contains(out, `mcp_tool="search"`), "tools never split these metrics")
	assert.False(t, strings.Contains(out, "not-a-tenant"))
}

func TestRecordHTTPLookup(t *testing.T) {
	ctx := WithTenant(context.Background(), "eeeeeeeeeeeeeeeeeeeeeeee")
	limited := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"X-Ratelimit-Remaining": {"0"}}}
	RecordHTTPLookup(ctx, LookupForgeGitHub, time.Now(), limited, nil)
	RecordHTTPLookup(ctx, LookupForgeGitLab, time.Now(), &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}, nil)
	RecordHTTPLookup(ctx, LookupForgeForgejo, time.Now(), nil, fmt.Errorf("get: %w", timeoutErr{}))

	out := text(t, Default)
	const id = `tenant_id="eeeeeeeeeeeeeeeeeeeeeeee"`
	assert.Contains(t, out, `codesynapse_lookups_total{service="forge_github",outcome="rate_limited",`+id)
	assert.Contains(t, out, `codesynapse_lookups_total{service="forge_gitlab",outcome="error",`+id)
	assert.Contains(t, out, `codesynapse_lookups_total{service="forge_forgejo",outcome="timeout",`+id)
}

func TestAuditSource(t *testing.T) {
	ctx := context.Background()
	assert.Equal(t, AuditSource(ctx), AuditService)
	assert.Equal(t, AuditSource(WithAuditSource(ctx, AuditAPI)), AuditAPI)
	assert.Equal(t, AuditSource(WithAuditSource(ctx, "../etc")), AuditService)
}
