package usage

// Agent memory counters, in the same daily buckets as the hook counters
// (Calls holds the count; they are never MCP calls): what the hooks do on
// their own, which the MCP tool counts do not see.
const (
	// MemoryAutoRecalls counts the per-prompt lookups of the chat's memory,
	// MemoryAutoRecallContext the tokens of context they added.
	MemoryAutoRecalls       = "(memory:auto-recalls)"        //nolint:gosec // daily counter identifier, not a credential
	MemoryAutoRecallContext = "(memory:auto-recall-context)" //nolint:gosec // daily counter identifier, not a credential
	// MemoryOffloads counts conversations handed to offloading (compaction
	// hook or offload_turns), MemoryOffloadTurns their turns and
	// MemoryOffloadFacts the facts the local model extracted from them.
	MemoryOffloads     = "(memory:offloads)"        //nolint:gosec // daily counter identifier, not a credential
	MemoryOffloadTurns = "(memory:offloaded-turns)" //nolint:gosec // daily counter identifier, not a credential
	MemoryOffloadFacts = "(memory:offload-facts)"   //nolint:gosec // daily counter identifier, not a credential
)

// IsMemoryCounter reports whether tool is one of these counters.
func IsMemoryCounter(tool string) bool {
	switch tool {
	case MemoryAutoRecalls, MemoryAutoRecallContext, MemoryOffloads, MemoryOffloadTurns, MemoryOffloadFacts:
		return true
	}
	return false
}
