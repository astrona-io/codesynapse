package usage

import "strings"

// Client families (docs/plans/PLATFORM_STATS_PLAN.md, "Usage and traffic"):
// client names are free strings a client reports about itself, so the
// platform statistics and the daily rollup name only these families. A
// client that names none of them is "other"; one that sent no name is
// "unknown".
const (
	ClientOther   = "other"
	ClientUnknown = "unknown"
)

// knownClients are the clients `codesynapse mcp install` registers with.
var knownClients = []string{"claude", "codex", "cursor", "gemini", "opencode"}

// ClientFamilies are the families in a stable order: the known clients,
// then other and unknown.
var ClientFamilies = append(append([]string{}, knownClients...), ClientOther, ClientUnknown)

// ClientFamily is the family of a client name as recorded in a bucket.
func ClientFamily(client string) string {
	c := strings.ToLower(strings.TrimSpace(client))
	if c == "" || c == ClientUnknown {
		return ClientUnknown
	}
	for _, f := range knownClients {
		if strings.Contains(c, f) {
			return f
		}
	}
	return ClientOther
}
