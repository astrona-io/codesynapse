package api

import "github.com/astrona-io/codesynapse/internal/usage"

// Token savings without the prompt cache (docs/PLATFORM_STATS_PLAN.md):
// the share of the agent's own work that CodeSynapse answers avoided. W,
// the work tokens, is fresh input + cache writes + output; cache reads are
// left out because CodeSynapse neither causes nor removes them. A is the
// avoided figure of clients that tell repeated ranges apart, scaled down
// by their repeat share. Share = A / (W + A).

// Reasons the share is or is not known, for share_avoided_basis.
const (
	shareBasisWork        = "work_tokens"
	shareBasisNoTokens    = "no_hook_tokens"
	shareBasisCacheFields = "cache_fields_missing"
	shareBasisNoRepeats   = "repeats_not_reported"
)

// Prices the saving can use for the first entry of avoided context.
const (
	savedPriceCacheWrite = "cache_write"
	savedPriceInput      = "input"
)

// SavedEstimate is what the avoided context in the share would have cost at
// the reference cloud model, valued once at the price of its first entry.
// Dollars is nil when that price is missing or no avoided tokens can be
// valued (unknown, never zero). UpperDollars adds the re-sent context at the
// cache-read price; it is shown apart and never added to totals.
type SavedEstimate struct {
	Tokens       int      `json:"tokens"`
	Price        string   `json:"price"`
	PerMTok      float64  `json:"per_mtok"`
	Dollars      *float64 `json:"dollars"`
	ResendTokens int      `json:"resend_tokens"`
	UpperDollars *float64 `json:"upper_dollars"`
	// UnawareTokens were avoided by clients that do not report repeats;
	// they are left out of Tokens and Dollars.
	UnawareTokens int `json:"unaware_tokens"`
}

type shareTool struct{ day, org, client, tool string }

type shareClient struct{ day, org, client string }

type shareGroup struct{ day, org, repo, client string }

type shareAvoided struct{ unscaled, response float64 }

// shareInputs gathers, before the main pass over the buckets, what the share
// needs: which clients report repeats, their repeat and returned ranges and
// re-sends per tool, and the hook token counters per group.
type shareInputs struct {
	aware    map[shareClient]bool
	returned map[shareTool]int
	repeats  map[shareTool]int
	resend   map[shareTool]int
	groups   map[shareGroup]*usage.TokenCounts
	// Filled during the main pass.
	avoided     map[shareTool]*shareAvoided
	unaware     float64
	share       float64
	shareLocal  float64
	repeatTotal int
}

func newShareInputs(buckets []usage.Bucket, inWindow func(day string) bool) *shareInputs {
	sh := &shareInputs{aware: map[shareClient]bool{}, returned: map[shareTool]int{}, repeats: map[shareTool]int{},
		resend: map[shareTool]int{}, groups: map[shareGroup]*usage.TokenCounts{}, avoided: map[shareTool]*shareAvoided{}}
	for _, bk := range buckets {
		if !inWindow(bk.Day) {
			continue
		}
		if bk.Tool == usage.RepeatAware {
			sh.aware[shareClient{bk.Day, bk.Org, bk.Client}] = true
			continue
		}
		if tool, ok := usage.RepeatToolOf(bk.Tool); ok {
			sh.repeats[shareTool{bk.Day, bk.Org, bk.Client, tool}] += bk.Calls
			sh.repeatTotal += bk.Calls
			continue
		}
		if tool, ok := usage.ResendToolOf(bk.Tool); ok {
			sh.resend[shareTool{bk.Day, bk.Org, bk.Client, tool}] += bk.Calls
			continue
		}
		g := shareGroup{bk.Day, bk.Org, bk.Repo, bk.Client}
		t := sh.groups[g]
		if t == nil {
			t = &usage.TokenCounts{}
		}
		if t.AddCounter(bk.Tool, bk.Calls) {
			sh.groups[g] = t
			continue
		}
		if bk.RangesReturned > 0 {
			sh.returned[shareTool{bk.Day, bk.Org, bk.Client, bk.Tool}] += bk.RangesReturned
		}
	}
	return sh
}

// shareToolOf is the tool a bucket's avoided figure belongs to: ask_local's
// avoided tokens arrive in their own counter.
func shareToolOf(bk usage.Bucket) shareTool {
	tool := bk.Tool
	if tool == usage.LocalAvoided {
		tool = "ask_local"
	}
	return shareTool{bk.Day, bk.Org, bk.Client, tool}
}

// scale returns a bucket's avoided figure after its repeats are taken out,
// and records it for the share. A client that does not report repeats keeps
// its figure but adds nothing to the share.
func (sh *shareInputs) scale(bk usage.Bucket, avoided float64, local bool) int {
	k := shareToolOf(bk)
	if !sh.aware[shareClient{k.day, k.org, k.client}] {
		sh.unaware += avoided
		return int(avoided)
	}
	sh.of(k).unscaled += avoided
	if rep := sh.repeats[k]; rep > 0 {
		avoided *= float64(sh.returned[k]) / float64(sh.returned[k]+rep)
	}
	sh.share += avoided
	if local {
		sh.shareLocal += avoided
	}
	return int(avoided)
}

// response records the tokens an MCP call returned, the base of its tool's
// avoided-per-response-token rate.
func (sh *shareInputs) response(bk usage.Bucket) {
	if bk.ResponseTokens > 0 {
		sh.of(shareToolOf(bk)).response += float64(bk.ResponseTokens)
	}
}

func (sh *shareInputs) of(k shareTool) *shareAvoided {
	av := sh.avoided[k]
	if av == nil {
		av = &shareAvoided{}
		sh.avoided[k] = av
	}
	return av
}

// resendTokens converts the hooks' answer tokens × later turns into avoided
// re-sent tokens with each tool's own avoided-per-response-token rate.
func (sh *shareInputs) resendTokens() float64 {
	var r float64
	for k, n := range sh.resend {
		av := sh.avoided[k]
		if av == nil || av.response <= 0 {
			continue
		}
		r += float64(n) * av.unscaled / av.response
	}
	return r
}

// cacheKnown reports whether every hook token group of the window reported
// its cache fields on every turn, and why not.
func (sh *shareInputs) cacheKnown() (bool, string) {
	seen := false
	for _, t := range sh.groups {
		if *t == (usage.TokenCounts{}) {
			continue
		}
		seen = true
		if t.Turns == 0 || t.CacheKnownTurns != t.Turns {
			return false, shareBasisCacheFields
		}
	}
	if !seen {
		return false, shareBasisNoTokens
	}
	return true, shareBasisWork
}

// finish fills the share fields of a from the gathered inputs.
func (sh *shareInputs) finish(a *AgentStats) {
	t := a.Tokens.TokenCounts
	a.WorkTokens = t.Work()
	a.ShareAvoidedTokens = int(sh.share)
	a.ShareLocalAvoidedTokens = int(sh.shareLocal)
	a.AvoidedUnawareTokens = int(sh.unaware)
	a.AvoidedRepeatRanges = sh.repeatTotal
	a.AvoidedResendTokens = int(sh.resendTokens())
	known, basis := sh.cacheKnown()
	if known && a.ShareAvoidedTokens == 0 && a.AvoidedUnawareTokens > 0 {
		known, basis = false, shareBasisNoRepeats
	}
	if known && a.WorkTokens+a.ShareAvoidedTokens == 0 {
		known, basis = false, shareBasisNoTokens
	}
	a.ShareAvoidedKnown, a.ShareAvoidedBasis = known, basis
	if !known {
		return
	}
	A, W, C, R := float64(a.ShareAvoidedTokens), float64(a.WorkTokens), float64(t.CachedInput), float64(a.AvoidedResendTokens)
	main := round1(100 * A / (W + A))
	a.ShareAvoidedPct = &main
	// The upper figure has cache reads on both sides; it is never shown
	// below the main one.
	upper := max(main, round1(100*(A+R)/(W+C+A+R)))
	a.ShareUpperPct = &upper
	if d := float64(t.Input+t.Output) + A; d > 0 {
		raw := round1(100 * A / d)
		a.ShareRawPct = &raw
	}
}

// savedEstimate values the graph part of the share's avoided tokens once, at
// the reference model's cache-write price when its provider caches by
// default and its input price otherwise. ask_local is valued with the local
// models, and the provider's own prompt-cache saving (llm_savings) is never
// part of it.
func savedEstimate(a AgentStats, ref PriceReference) SavedEstimate {
	e := SavedEstimate{Tokens: max(0, a.ShareAvoidedTokens-a.ShareLocalAvoidedTokens), ResendTokens: a.AvoidedResendTokens,
		UnawareTokens: a.AvoidedUnawareTokens, Price: savedPriceInput, PerMTok: ref.InputPerMTok}
	if ref.CachesByDefault {
		e.Price, e.PerMTok = savedPriceCacheWrite, ref.CacheWritePerMTok
	}
	if e.PerMTok <= 0 || (e.Tokens == 0 && e.UnawareTokens > 0) {
		return e
	}
	d := round4(float64(e.Tokens) * e.PerMTok / 1e6)
	e.Dollars = &d
	if ref.CachedInputPerMTok > 0 {
		u := round4(d + float64(e.ResendTokens)*ref.CachedInputPerMTok/1e6)
		e.UpperDollars = &u
	}
	return e
}
