package usage

// ask_local counters, in the same daily buckets as the hook counters
// (Calls holds the count; they are never MCP calls). LocalRead is what the
// local model read; LocalAvoided is what the agent did not read: the files'
// tokens (bytes/4) minus the answer it got instead.
const (
	LocalRead    = "(local:read)"    //nolint:gosec // daily counter identifier, not a credential
	LocalAvoided = "(local:avoided)" //nolint:gosec // daily counter identifier, not a credential
	// LocalBriefs counts prompts a local model briefed the agent on.
	LocalBriefs = "(local:briefs)" //nolint:gosec // daily counter identifier, not a credential
)
