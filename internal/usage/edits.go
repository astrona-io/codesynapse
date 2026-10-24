package usage

import "fmt"

// EditCounts measures editing payload sizes, never source content. AvoidedBytes
// compares successful edits with a hypothetical full rewrite of that file.
type EditCounts struct {
	Edits         int `json:"edits"`
	Writes        int `json:"writes"`
	Failed        int `json:"failed"`
	PatchBytes    int `json:"patch_bytes"`
	WriteBytes    int `json:"write_bytes"`
	BaselineBytes int `json:"baseline_bytes"`
	AvoidedBytes  int `json:"avoided_bytes"`
}

// Counters returns metadata-only daily counters, excluded from MCP calls.
func (e EditCounts) Counters() map[string]int {
	return map[string]int{"(edit:edits)": e.Edits, "(edit:writes)": e.Writes, "(edit:failed)": e.Failed,
		"(edit:patch-bytes)": e.PatchBytes, "(edit:write-bytes)": e.WriteBytes,
		"(edit:baseline-bytes)": e.BaselineBytes, "(edit:avoided-bytes)": e.AvoidedBytes}
}

// Validate bounds every client-supplied count and its rewrite estimate.
func (e EditCounts) Validate() error {
	for _, n := range e.Counters() {
		if n < 0 || n > MaxHookTokens {
			return fmt.Errorf("edit counts must be between 0 and %d", MaxHookTokens)
		}
	}
	if e.Edits > 100_000 || e.Writes > 100_000 || e.Failed > e.Edits+e.Writes || e.AvoidedBytes > e.BaselineBytes {
		return fmt.Errorf("invalid edit counts or rewrite baseline")
	}
	return nil
}

// AddCounter accumulates a recognised edit counter.
func (e *EditCounts) AddCounter(tool string, n int) bool {
	switch tool {
	case "(edit:edits)":
		e.Edits += n
	case "(edit:writes)":
		e.Writes += n
	case "(edit:failed)":
		e.Failed += n
	case "(edit:patch-bytes)":
		e.PatchBytes += n
	case "(edit:write-bytes)":
		e.WriteBytes += n
	case "(edit:baseline-bytes)":
		e.BaselineBytes += n
	case "(edit:avoided-bytes)":
		e.AvoidedBytes += n
	default:
		return false
	}
	return true
}
