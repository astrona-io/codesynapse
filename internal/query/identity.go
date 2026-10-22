package query

import (
	"context"
	"fmt"
	"strings"

	"github.com/astrona-io/codesynapse/internal/identity"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
)

// IdentityPage is shared by the API and MCP. Available distinguishes an older
// index from an empty result; historical symbols are explicitly inactive.
type IdentityPage struct {
	Repo      string            `json:"repo"`
	Revision  string            `json:"revision,omitempty"`
	IndexedAt string            `json:"indexed_at,omitempty"`
	Available bool              `json:"available"`
	Symbols   []identity.Symbol `json:"symbols"`
	Total     int               `json:"total"`
	Offset    int               `json:"offset"`
	Omitted   int               `json:"omitted"`
	Truncated bool              `json:"truncated"`
}

// IdentityReport reads only the selected repository through its scope fence.
func IdentityReport(ctx context.Context, r store.Reader, repo string) (identity.Report, bool, error) {
	r = store.ForRepository(r, repo)
	n, err := r.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		return identity.Report{}, false, err
	}
	report, err := identity.Decode(n.Identities, repo)
	if err == nil {
		report, err = filterIdentityPolicy(report, n.Meta)
	}
	return report, n.Identities != "", err
}

// Identities lists identities, optionally one permanent ID or unresolved
// matches only. Each response is bounded, with an explicit omitted count.
func (s Service) Identities(ctx context.Context, repo, id string, reviewOnly bool, offset, limit int) (IdentityPage, error) {
	r, available, err := IdentityReport(ctx, s.R, repo)
	page := IdentityPage{Repo: repo, Available: available, Revision: r.Revision, IndexedAt: r.IndexedAt, Symbols: []identity.Symbol{}, Offset: max(offset, 0)}
	if err != nil {
		return page, err
	}
	matches := []identity.Symbol{}
	for _, symbol := range r.Symbols {
		if id != "" && symbol.ID != id {
			continue
		}
		if reviewOnly && len(symbol.Candidates) == 0 && symbol.CandidatesOmitted == 0 {
			continue
		}
		matches = append(matches, symbol)
	}
	page.Total = len(matches)
	start := min(page.Offset, len(matches))
	end := min(start+store.ClampLimit(limit, 20), len(matches))
	page.Symbols = matches[start:end]
	page.Omitted = len(matches) - (end - start)
	page.Truncated = page.Omitted > 0
	return page, nil
}

// ResolveSymbolID resolves only current symbols. Historical occurrences are
// available through Identities; they must not be returned as current code.
func ResolveSymbolID(ctx context.Context, r store.Reader, repo, id string) (model.Node, error) {
	r = store.ForRepository(r, repo)
	report, _, err := IdentityReport(ctx, r, repo)
	if err != nil {
		return model.Node{}, err
	}
	for _, s := range report.Symbols {
		if s.ID == id && s.Active {
			n, err := r.Node(ctx, s.Current.NodeID)
			if err != nil {
				return model.Node{}, err
			}
			if n.SymbolID != id {
				return model.Node{}, fmt.Errorf("symbol identity projection mismatch")
			}
			return n, nil
		}
	}
	return model.Node{}, fmt.Errorf("symbol %q: %w", id, store.ErrNotFound)
}

// IsSymbolID distinguishes permanent IDs from human-readable locators.
func IsSymbolID(s string) bool { return strings.HasPrefix(s, "sym-") && len(s) == 36 }
