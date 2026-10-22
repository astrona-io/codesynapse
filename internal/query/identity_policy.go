package query

import (
	"encoding/json"
	"fmt"

	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/identity"
)

// The private baseline retains tombstones for matching; public identity reads
// must not expose paths excluded by the active ingestion policy.
func filterIdentityPolicy(report identity.Report, meta string) (identity.Report, error) {
	if meta == "" {
		return report, nil
	}
	var document struct {
		Policy struct {
			Present bool            `json:"present"`
			Digest  string          `json:"digest"`
			Config  json.RawMessage `json:"config"`
		} `json:"repository_policy"`
	}
	if err := json.Unmarshal([]byte(meta), &document); err != nil {
		return identity.Report{}, fmt.Errorf("decode identity policy: %w", err)
	}
	if !document.Policy.Present {
		return report, nil
	}
	policy, err := repoconfig.ParseCanonicalJSON(document.Policy.Config)
	if err != nil || policy.Digest() != document.Policy.Digest {
		return identity.Report{}, fmt.Errorf("identity policy does not match indexed digest")
	}
	if len(policy.Config().Ignore.Paths) == 0 {
		return report, nil
	}
	visible := map[string]bool{}
	symbols := make([]identity.Symbol, 0, len(report.Symbols))
	for _, symbol := range report.Symbols {
		if symbol.Current.File != "" && !policy.PathAllowed(symbol.Current.File) {
			continue
		}
		// Removed packages have no file evidence that can establish eligibility.
		if symbol.Current.File == "" && !symbol.Active {
			continue
		}
		history := make([]identity.Location, 0, len(symbol.History))
		for _, location := range symbol.History {
			if location.File != "" && !policy.PathAllowed(location.File) {
				symbol.HistoryOmitted++
				continue
			}
			history = append(history, location)
		}
		symbol.History = history
		visible[symbol.ID] = true
		symbols = append(symbols, symbol)
	}
	for i := range symbols {
		candidates := make([]string, 0, len(symbols[i].Candidates))
		for _, candidate := range symbols[i].Candidates {
			if visible[candidate] {
				candidates = append(candidates, candidate)
			} else {
				symbols[i].CandidatesOmitted++
			}
		}
		symbols[i].Candidates = candidates
	}
	report.Symbols = symbols
	return report, nil
}
