package usage

// fetch_source counters, in the same daily buckets as the hook counters
// (Calls holds the count; they are never MCP calls), from the bridge's
// report of each page it returned: no URL, host or question, and no
// repository (a page has nothing to do with the checkout).
const (
	// SourceFetches counts pages returned, SourceHits those of them the
	// cache answered (fresh or after a 304).
	SourceFetches = "(source:fetches)"
	SourceHits    = "(source:hits)"
	// SourceRaw is the estimated tokens of the pages as served, and
	// SourceSaved what the agent did not read: raw minus the text returned,
	// per page and never below zero.
	SourceRaw   = "(source:raw)"
	SourceSaved = "(source:saved)"
)
