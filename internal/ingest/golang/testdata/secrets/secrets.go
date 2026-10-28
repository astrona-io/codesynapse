// Package secrets holds constants named like secrets; only some are.
package secrets

// Usage counter names (internal/usage/tokens.go, 2026-10-28): labels, not
// credentials. They must not be reported.
const (
	TokenInput  = "(tokens:input)"
	TokenCached = "(tokens:cached-input)"
	TokenTurns  = "(tokens:cache-known-turns)"
)

// Values that still look like credentials and must be reported.
const (
	apiToken      = "s3cr3t-value-123456"
	sessionToken  = "(tokens:Abc123XyZ)"
	adminPassword = "correct-horse-battery-staple"
)
