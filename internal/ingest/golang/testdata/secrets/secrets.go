// Package secrets holds constants named like secrets; only some are.
package secrets

// Usage counter names (internal/usage/tokens.go, 2026-10-28): labels, not
// credentials. They must not be reported.
const (
	TokenInput  = "(tokens:input)"
	TokenCached = "(tokens:cached-input)"
	TokenTurns  = "(tokens:cache-known-turns)"
)

// An enum value of lowercase words (services/api/internal/api/stats_share.go,
// 2026-10-28) is not a credential either.
const basisNoTokens = "no_hook_tokens"

// Values that still look like credentials and must be reported.
const (
	apiToken      = "s3cr3t-value-123456"
	sessionToken  = "(tokens:Abc123XyZ)"
	adminPassword = "correct-horse-battery-staple"
	mixedToken    = "ab12_cd34_ef56"
	casedSecret   = "no_Hook_tokens"
)
