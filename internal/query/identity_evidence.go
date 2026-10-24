package query

import (
	"context"

	"github.com/astrona-io/codesynapse/internal/identity"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
)

// IdentityFileVisibility applies the same policy to saved review evidence as
// to current identity locations, including paths no longer in retained history.
func IdentityFileVisibility(ctx context.Context, r store.Reader, repo string, paths []string) (map[string]bool, error) {
	n, err := store.ForRepository(r, repo).Node(ctx, model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		return nil, err
	}
	evidence := identity.Report{}
	for _, path := range paths {
		evidence.Symbols = append(evidence.Symbols, identity.Symbol{ID: path, Active: true, Current: identity.Location{File: path}})
	}
	filtered, err := filterIdentityPolicy(evidence, n.Meta)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, s := range filtered.Symbols {
		allowed[s.ID] = true
	}
	return allowed, nil
}
