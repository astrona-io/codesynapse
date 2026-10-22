// Package arch reads the architecture records a repository keeps as files:
// ADRs, RFCs, standards, policies, overviews, runbooks and diagrams. It
// reads their metadata (type, status, owner, scope, supersession, the code they
// name) and where they are, not their text, and turns it into Record and
// Component nodes with edges to each other and to code.
//
// A record is a document, and not every document is a requirement: only an
// accepted ADR or an active standard or policy is binding (see Binding).
// Proposals, drafts, rejected and superseded records are kept, labelled as
// such, and never presented as a rule.
package arch

import "strings"

// Record types.
const (
	ADR      = "adr"
	RFC      = "rfc"
	Standard = "standard"
	Policy   = "policy"
	Overview = "overview"
	Runbook  = "runbook"
	Diagram  = "diagram"
)

// Types lists the record types.
var Types = []string{ADR, RFC, Standard, Policy, Overview, Runbook, Diagram}

// Statuses in the words of each type's lifecycle.
var statusesOf = map[string][]string{
	ADR:      {"proposed", "accepted", "rejected", "deprecated", "superseded"},
	RFC:      {"draft", "in-review", "accepted", "rejected", "withdrawn", "implemented", "superseded"},
	Standard: {"draft", "active", "deprecated", "retired", "superseded"},
	Policy:   {"draft", "active", "deprecated", "retired", "superseded"},
	Overview: {"draft", "current", "outdated", "retired"},
	Runbook:  {"draft", "current", "outdated", "retired"},
	Diagram:  {"draft", "current", "outdated", "retired"},
}

// Unknown is the status of a record whose status text matches no word of its type.
const Unknown = "unknown"

// StatusesOf returns the lifecycle of a record type.
func StatusesOf(docType string) []string { return statusesOf[docType] }

var synonyms = map[string]string{
	"proposed": "proposed", "proposal": "proposed", "draft": "draft", "wip": "draft", "work in progress": "draft",
	"accepted": "accepted", "approved": "accepted", "adopted": "accepted", "decided": "accepted", "agreed": "accepted",
	"active": "active", "current": "current", "enforced": "active", "in force": "active",
	"rejected": "rejected", "declined": "rejected", "denied": "rejected",
	"deprecated": "deprecated", "obsolete": "retired", "retired": "retired", "withdrawn": "withdrawn", "abandoned": "withdrawn",
	"superseded": "superseded", "replaced": "superseded",
	"in review": "in-review", "in-review": "in-review", "review": "in-review", "under review": "in-review", "open": "in-review",
	"implemented": "implemented", "done": "implemented", "final": "implemented",
	"outdated": "outdated", "stale": "outdated",
}

// Canonical maps the status as written to a word of the type's lifecycle. ok is
// false when it does not belong (an ADR cannot be "current"): the record then
// has status Unknown, and the original text is kept in its metadata.
func Canonical(docType, raw string) (status string, ok bool) {
	s := strings.ToLower(strings.TrimSpace(strings.Trim(strings.NewReplacer("*", "", "_", "", "`", "").Replace(strings.TrimSpace(raw)), ".")))
	// "Accepted (2026-10-22)" and "accepted - see ADR 5" start with the word.
	for _, sep := range []string{" (", " - ", " — ", ",", ";", " by "} {
		if i := strings.Index(s, sep); i > 0 {
			s = s[:i]
		}
	}
	// Informal wording some formats use for the same state.
	if docType == Overview || docType == Runbook || docType == Diagram {
		switch s {
		case "accepted", "approved", "active", "adopted":
			s = "current"
		case "proposed":
			s = "draft"
		case "deprecated":
			s = "retired"
		}
	}
	if docType == Standard || docType == Policy {
		switch s {
		case "accepted", "approved", "adopted", "current":
			s = "active"
		case "proposed":
			s = "draft"
		}
	}
	if docType == RFC && s == "proposed" {
		s = "draft"
	}
	c, found := synonyms[s]
	if !found {
		return Unknown, false
	}
	for _, allowed := range statusesOf[docType] {
		if allowed == c {
			return c, true
		}
	}
	return Unknown, false
}

// Binding reports whether a record in this state is a rule agents and people
// must follow: an accepted ADR, or an active standard or policy. Everything
// else (drafts, proposals, rejected, deprecated and superseded records,
// overviews, runbooks, diagrams, RFCs) informs but does not bind.
func Binding(docType, status string) bool {
	switch docType {
	case ADR:
		return status == "accepted"
	case Standard, Policy:
		return status == "active"
	}
	return false
}

// Meta is the part of a record that does not need its own node property. It is
// stored as JSON in Node.Meta.
type Meta struct {
	// ID is the record's stable ID ("ADR-0001"): front matter id, else the
	// number of its file name, else derived from its path.
	ID        string `json:"id"`
	RawStatus string `json:"raw_status,omitempty"`
	// Format is how the metadata was found: front-matter, bullets or section.
	Format string `json:"format,omitempty"`
	Owner  string `json:"owner,omitempty"`
	Date   string `json:"date,omitempty"`
	// Topic names what the record decides, so two records deciding the same
	// topic for one component can be compared.
	Topic      string   `json:"topic,omitempty"`
	Labels     []string `json:"labels,omitempty"`
	Components []string `json:"components,omitempty"`
	// Supersedes, SupersededBy, Relates and Conflicts hold record IDs.
	Supersedes   []string `json:"supersedes,omitempty"`
	SupersededBy []string `json:"superseded_by,omitempty"`
	Relates      []string `json:"relates,omitempty"`
	Conflicts    []string `json:"conflicts_with,omitempty"`
	// Implements lists the code the record names, with what each resolved to.
	Implements []CodeRef `json:"implements,omitempty"`
	// Unresolved says what could not be matched (an ID that no record has, a
	// symbol that is missing or ambiguous).
	Unresolved []string `json:"unresolved,omitempty"`
}

// CodeRef is one code reference of a record and the nodes it matched.
type CodeRef struct {
	Ref      string   `json:"ref"`
	Resolved []string `json:"resolved,omitempty"`
}
