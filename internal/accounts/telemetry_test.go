package accounts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestAuthOutcome(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, telemetry.AuthOK},
		{fmt.Errorf("login: %w", ErrUnauthorized), telemetry.AuthBadCredentials},
		{ErrRateLimited, telemetry.AuthRateLimited},
		{ErrSuspended, telemetry.AuthSuspended},
		{ErrKeyRevoked, telemetry.AuthExpired},
		{ErrSSOState, telemetry.AuthExpired},
		{ErrForbidden, telemetry.AuthRefused},
		{ErrSSONoAccess, telemetry.AuthRefused},
		{ErrSSOLinkRequired, telemetry.AuthRefused},
		{ErrRootAccount, telemetry.AuthRefused},
		{errors.New("connection reset"), ""},
	}
	for _, c := range cases {
		assert.Equal(t, authOutcome(c.err), c.want, fmt.Sprint(c.err))
	}
}

type eventsOnly struct {
	Store
	err error
}

func (s eventsOnly) AddEvent(_ context.Context, e Event) (Event, error) { return e, s.err }

// Audit writes count for the tenant and source on the context, never for
// the organisation the event names.
func TestRecordCountsAuditWrites(t *testing.T) {
	const tenant = "efefefefefefefefefefefef"
	const named = "fefefefefefefefefefefefe"
	ctx := telemetry.WithAuditSource(telemetry.WithTenant(context.Background(), tenant), telemetry.AuditAPI)
	ok := &Service{St: eventsOnly{}, Now: time.Now}
	_, err := ok.Record(ctx, Event{Action: "repo.add", OrgID: named})
	require.NoError(t, err)
	broken := &Service{St: eventsOnly{err: errors.New("disk full")}, Now: time.Now}
	_, err = broken.Record(telemetry.WithAuditSource(context.Background(), telemetry.AuditMCP), Event{Action: "mcp.tools/call", OrgID: named})
	require.Error(t, err)

	var b strings.Builder
	require.NoError(t, telemetry.Default.WriteText(&b))
	out := b.String()
	assert.Contains(t, out, `codesynapse_audit_writes_total{source="api",outcome="ok",tenant_id="`+tenant+`"`)
	assert.Contains(t, out, `codesynapse_audit_write_seconds_count{source="api",tenant_id="`+tenant+`"`)
	assert.Contains(t, out, `platform_codesynapse_audit_writes_total{source="mcp",outcome="error"`)
	assert.False(t, strings.Contains(out, named))
}

// The tenant comes from the server-resolved credential, and only when the
// check succeeded; whatever tenant the context already carried is replaced.
func TestRecordAuthTenant(t *testing.T) {
	const org = "abababababababababababab"
	const other = "cdcdcdcdcdcdcdcdcdcdcdcd"
	ctx := telemetry.WithTenant(context.Background(), other)
	RecordAuth(ctx, telemetry.AuthSSO, org, nil)
	RecordAuth(ctx, telemetry.AuthSSO, org, ErrSSONoAccess)
	RecordAuth(ctx, telemetry.AuthAPIKey, "", errors.New("store down"))

	var b strings.Builder
	require.NoError(t, telemetry.Default.WriteText(&b))
	out := b.String()
	assert.Contains(t, out, `codesynapse_auth_attempts_total{method="sso",outcome="ok",tenant_id="`+org+`"`)
	assert.Contains(t, out, `platform_codesynapse_auth_attempts_total{method="sso",outcome="refused"`)
	assert.False(t, strings.Contains(out, other), "a failed check never counts for the context's tenant")
	assert.False(t, strings.Contains(out, `method="api_key"`), "errors that say nothing about the credential are not counted")
}
