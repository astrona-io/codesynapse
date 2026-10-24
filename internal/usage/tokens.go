package usage

import "fmt"

// Hook token counters use the same daily, tenant-scoped buckets as workflow
// counts. Calls holds the count in these buckets; they are never MCP calls.
const (
	TokenInput    = "(tokens:input)"        //nolint:gosec // daily counter identifier, not a credential
	TokenOutput   = "(tokens:output)"       //nolint:gosec // daily counter identifier, not a credential
	TokenCached   = "(tokens:cached-input)" //nolint:gosec // daily counter identifier, not a credential
	TokenTurns    = "(tokens:turns)"        //nolint:gosec // daily counter identifier, not a credential
	MaxHookTokens = 1_000_000_000
)

// TokenCounts contains model-reported usage, not a text-size estimate.
// Input includes cache reads and writes; CachedInput is a subset of Input.
type TokenCounts struct {
	Input       int `json:"input"`
	Output      int `json:"output"`
	CachedInput int `json:"cached_input"`
	Turns       int `json:"turns"`
}

// Validate bounds hook-supplied counters and checks the cached-input subset.
func (t TokenCounts) Validate() error {
	for _, n := range []int{t.Input, t.Output, t.CachedInput, t.Turns} {
		if n < 0 || n > MaxHookTokens {
			return fmt.Errorf("token counts must be between 0 and %d", MaxHookTokens)
		}
	}
	if t.CachedInput > t.Input || t.Turns > 100_000 {
		return fmt.Errorf("invalid cached input or reported turn count")
	}
	return nil
}

// Counters returns the daily pseudo-tool counters to persist.
func (t TokenCounts) Counters() map[string]int {
	return map[string]int{TokenInput: t.Input, TokenOutput: t.Output, TokenCached: t.CachedInput, TokenTurns: t.Turns}
}

// AddCounter accumulates a token counter and reports whether tool names one.
func (t *TokenCounts) AddCounter(tool string, count int) bool {
	switch tool {
	case TokenInput:
		t.Input += count
	case TokenOutput:
		t.Output += count
	case TokenCached:
		t.CachedInput += count
	case TokenTurns:
		t.Turns += count
	default:
		return false
	}
	return true
}
