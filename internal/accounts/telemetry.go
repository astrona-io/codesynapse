package accounts

import (
	"context"
	"errors"

	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// authOutcome maps what a credential check returned to the outcome label of
// codesynapse_auth_attempts_total, from sentinel errors only. Errors that say
// nothing about the credential (a store that is down) return "" and are not
// counted.
func authOutcome(err error) string {
	switch {
	case err == nil:
		return telemetry.AuthOK
	case errors.Is(err, ErrRateLimited):
		return telemetry.AuthRateLimited
	case errors.Is(err, ErrSuspended):
		return telemetry.AuthSuspended
	case errors.Is(err, ErrKeyRevoked), errors.Is(err, ErrSSOState):
		return telemetry.AuthExpired
	case errors.Is(err, ErrRootAccount), errors.Is(err, ErrForbidden), errors.Is(err, ErrSSONoAccess), errors.Is(err, ErrSSOLinkRequired):
		return telemetry.AuthRefused
	case errors.Is(err, ErrUnauthorized):
		return telemetry.AuthBadCredentials
	}
	return ""
}

// RecordAuth counts one credential check by method (telemetry.AuthPassword,
// AuthAPIKey, AuthSession, AuthSSO). A successful check counts for the
// organisation the credential itself is bound to (an API key's, the
// identity provider's), which the server resolved; every other check counts
// on the platform series, since nobody is trusted yet.
func RecordAuth(ctx context.Context, method, orgID string, err error) {
	outcome := authOutcome(err)
	if outcome == "" {
		return
	}
	if outcome == telemetry.AuthOK && orgID != "" {
		ctx = telemetry.WithTenant(ctx, orgID)
	} else {
		ctx = telemetry.WithTenant(ctx, "")
	}
	telemetry.RecordAuth(ctx, method, outcome)
}
