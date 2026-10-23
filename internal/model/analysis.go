package model

import (
	"encoding/json"
	"errors"
	"slices"
)

// Evidence says how an extractor established a resolution edge (CALLS,
// REFERENCES, IMPLEMENTS, DISPATCHES_TO). The empty value is unknown: edges
// written before AnalysisVersion 1, or by a producer that does not say.
// Readers must never treat unknown as typed.
type Evidence string

const (
	// EvidenceTyped edges were resolved by a type checker (go/types).
	EvidenceTyped Evidence = "typed"
	// EvidenceDataflow edges link a call through a function value to a
	// function whose value can reach it, found by type-based data-flow
	// analysis (SSA and VTA). The target is possible, not proven: the
	// analysis over-approximates.
	EvidenceDataflow Evidence = "dataflow"
	// EvidenceSyntactic edges were resolved from declarations and imports
	// in the source, without types: a same-file symbol, an imported name,
	// self/this inside a class.
	EvidenceSyntactic Evidence = "syntactic"
	// EvidenceInferred edges were guessed from a name that is unique in the
	// repository; the receiver's type was not known.
	EvidenceInferred Evidence = "inferred"
	// EvidenceUnknown is how readers name the empty value.
	EvidenceUnknown Evidence = "unknown"
)

// evidenceRank orders evidence from weakest to strongest; the empty value
// ranks lowest.
var evidenceRank = []Evidence{"", EvidenceInferred, EvidenceSyntactic, EvidenceDataflow, EvidenceTyped}

// Stronger reports whether e is better evidence than other.
func (e Evidence) Stronger(other Evidence) bool {
	return slices.Index(evidenceRank, e) > slices.Index(evidenceRank, other)
}

// Valid reports whether e may be stored: a known value or empty.
func (e Evidence) Valid() bool { return slices.Contains(evidenceRank, e) }

// OrUnknown returns e, or EvidenceUnknown for the empty value.
func (e Evidence) OrUnknown() Evidence {
	if e == "" {
		return EvidenceUnknown
	}
	return e
}

// ResolutionEdge reports whether edges of kind k carry Evidence.
func ResolutionEdge(k EdgeKind) bool {
	return k == EdgeCalls || k == EdgeReferences || k == EdgeImplements || k == EdgeDispatchesTo
}

// AnalysisVersion is the version of Edge.Evidence and Node.Analysis the
// extractors write. Readers accept older and newer versions: unknown fields
// are ignored, and a missing version means nothing was recorded.
const AnalysisVersion = 1

// Data-flow analysis outcomes (Analysis.Dataflow).
const (
	DataflowVTA     = "vta"
	DataflowSkipped = "skipped"
)

// Analysis levels.
const (
	LevelTyped     = "typed"
	LevelSyntactic = "syntactic"
)

// Analysis statuses. Complete means the extractor found no problem, which for
// a syntactic level is a weaker statement than for a typed one.
const (
	StatusComplete = "complete"
	// StatusPartial: the source had errors (type errors, unbalanced
	// brackets, an unterminated string), so declarations or edges may be
	// missing.
	StatusPartial = "partial"
	// StatusTruncated: a work budget was reached and some function bodies
	// were not analysed.
	StatusTruncated = "truncated"
)

// Analysis records how completely a Package, File, function or Repository
// was analysed and what was left out. It is stored JSON-encoded in
// Node.Analysis.
type Analysis struct {
	Version int    `json:"version"`
	Level   string `json:"level,omitempty"`
	Status  string `json:"status,omitempty"`
	// Errors is the number of load, parse or type errors.
	Errors int `json:"errors,omitempty"`
	// IgnoredFiles are source files the build configuration excluded
	// (build tags, GOOS/GOARCH), so their declarations are not in the graph.
	IgnoredFiles int `json:"ignored_files,omitempty"`
	// UnresolvedCalls are call sites whose target could not be determined:
	// calls through function values and closures. Calls into other
	// modules are not counted.
	UnresolvedCalls int `json:"unresolved_calls,omitempty"`
	// OmittedBodies are function bodies skipped after the work budget ran out.
	OmittedBodies int `json:"omitted_bodies,omitempty"`
	// Dataflow says whether the optional data-flow analysis ran: "vta" or
	// "skipped" (type errors, too large or failed). Empty when not enabled.
	Dataflow string `json:"dataflow,omitempty"`
	// DataflowCalls are unresolved call sites that data-flow analysis linked
	// to at least one function in the repository.
	DataflowCalls int `json:"dataflow_calls,omitempty"`

	// Repository totals.
	Partial   int              `json:"partial,omitempty"`
	Truncated int              `json:"truncated,omitempty"`
	Edges     map[Evidence]int `json:"edges,omitempty"`
}

// maxAnalysisBytes bounds what DecodeAnalysis accepts from the store.
const maxAnalysisBytes = 4 << 10

// Encode returns the JSON form, stamped with AnalysisVersion.
func (a Analysis) Encode() string {
	a.Version = AnalysisVersion
	b, err := json.Marshal(a)
	if err != nil { // only maps of strings to ints: cannot fail
		panic(err)
	}
	return string(b)
}

// DecodeAnalysis reads Node.Analysis. The empty string yields a zero
// Analysis (version 0: nothing recorded).
func DecodeAnalysis(s string) (Analysis, error) {
	var a Analysis
	if s == "" {
		return a, nil
	}
	if len(s) > maxAnalysisBytes {
		return Analysis{}, errors.New("analysis record too large")
	}
	if err := json.Unmarshal([]byte(s), &a); err != nil {
		return Analysis{}, err
	}
	return a, nil
}
