package usage

import "fmt"

// Hook token counters use the same daily, tenant-scoped buckets as workflow
// counts. Calls holds the count in these buckets; they are never MCP calls.
const (
	TokenInput      = "(tokens:input)"             //nolint:gosec // daily counter identifier, not a credential
	TokenOutput     = "(tokens:output)"            //nolint:gosec // daily counter identifier, not a credential
	TokenCached     = "(tokens:cached-input)"      //nolint:gosec // daily counter identifier, not a credential
	TokenCacheWrite = "(tokens:cache-write)"       //nolint:gosec // daily counter identifier, not a credential
	TokenTurns      = "(tokens:turns)"             //nolint:gosec // daily counter identifier, not a credential
	TokenCacheKnown = "(tokens:cache-known-turns)" //nolint:gosec // daily counter identifier, not a credential
	MaxHookTokens   = 1_000_000_000
)

// TokenCounts contains model-reported usage, not a text-size estimate.
// Input includes cache reads and writes; CachedInput (cache reads) and
// CacheWrite are disjoint subsets of Input, so fresh input is
// Input - CachedInput - CacheWrite. CacheKnownTurns counts the turns whose
// usage carried the cache fields: only when it equals Turns can cache reads
// be told apart from the agent's own work (docs/PLATFORM_STATS_PLAN.md).
type TokenCounts struct {
	Input           int `json:"input"`
	Output          int `json:"output"`
	CachedInput     int `json:"cached_input"`
	CacheWrite      int `json:"cache_write"`
	Turns           int `json:"turns"`
	CacheKnownTurns int `json:"cache_known_turns"`
}

// Validate bounds hook-supplied counters and checks the cache subsets.
func (t TokenCounts) Validate() error {
	for _, n := range []int{t.Input, t.Output, t.CachedInput, t.CacheWrite, t.Turns, t.CacheKnownTurns} {
		if n < 0 || n > MaxHookTokens {
			return fmt.Errorf("token counts must be between 0 and %d", MaxHookTokens)
		}
	}
	if t.CachedInput+t.CacheWrite > t.Input || t.Turns > 100_000 || t.CacheKnownTurns > t.Turns {
		return fmt.Errorf("invalid cached input, cache writes or reported turn count")
	}
	return nil
}

// Work is the agent's own work: fresh input, cache writes and output. Cache
// reads are left out; CodeSynapse neither causes nor removes them.
func (t TokenCounts) Work() int { return t.Input - t.CachedInput + t.Output }

// Counters returns the daily pseudo-tool counters to persist.
func (t TokenCounts) Counters() map[string]int {
	return map[string]int{TokenInput: t.Input, TokenOutput: t.Output, TokenCached: t.CachedInput, TokenCacheWrite: t.CacheWrite,
		TokenTurns: t.Turns, TokenCacheKnown: t.CacheKnownTurns}
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
	case TokenCacheWrite:
		t.CacheWrite += count
	case TokenTurns:
		t.Turns += count
	case TokenCacheKnown:
		t.CacheKnownTurns += count
	default:
		return false
	}
	return true
}
