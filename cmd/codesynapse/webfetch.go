package main

import (
	"fmt"
	"os"
	"slices"

	"github.com/astrona-io/codesynapse/internal/fetchsource"
)

// hook webfetch (PreToolUse on WebFetch, docs/TEXT_OPTIMIZATION_PLAN.md
// phase 6): when fetch_source already holds the page in its local cache, the
// first WebFetch of it in a session is turned back with a pointer to
// fetch_source, which answers from that cache without a download. A page
// fetch_source has not cached, a URL it would not fetch (not https, with
// credentials, unparsable) and a second WebFetch of a steered page go
// through: the hook nudges, it never stands between the agent and a page.
// The lookup only reads the cache folder; the URL is never sent or logged,
// and the session state keeps the cache key (a hash), not the URL.
// CODESYNAPSE_FETCH_STEER=false turns it off.

// maxSteered bounds the cache keys a session remembers.
const maxSteered = 200

// fetchSteer returns the PreToolUse answer for a WebFetch call, or nil to let
// it run.
func fetchSteer(in toolHookInput) map[string]any {
	if os.Getenv("CODESYNAPSE_FETCH_STEER") == "false" || in.ToolName != "" && in.ToolName != "WebFetch" {
		return nil
	}
	// Normalise refuses what fetch_source would refuse, so nothing is steered
	// to a tool that cannot answer for it.
	u, err := fetchsource.Normalise(in.ToolInput.URL)
	if err != nil {
		return nil
	}
	dir, err := fetchsource.DefaultDir()
	if err != nil || !fetchsource.Cached(dir, u) {
		return nil // a first fetch, or no cache to answer from
	}
	state := stateFile(in.SessionID)
	if state == "" {
		return nil // without state a second WebFetch could not get through
	}
	key := fetchsource.Key(u)
	s := loadState(state)
	if slices.Contains(s.Steered, key) {
		return nil // fetched again after the pointer: let it
	}
	s.Steered = append(s.Steered, key)
	s.Steers++
	saveState(state, s)
	reason := fmt.Sprintf("fetch_source already has this page in its local cache. Call fetch_source with url %q instead: "+
		"it answers from the cache (asking the site only whether a stale copy changed) with the page as plain text. "+
		"If fetch_source takes a question argument, pass what you want to learn from the page as question. "+
		"If fetch_source is not available, call WebFetch again.", u.String())
	return map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": reason,
	}}
}
