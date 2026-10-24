package agentctx

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
)

// Cross-repository impact (docs/PLAN.md): a changed exported Go function of
// a library, and where other indexed repositories call it. Ingest keeps
// External nodes for calls into modules of the repository's own owner
// (golang.LinkFirstParty), so the callers are one node lookup and one hop
// away in each repository that requires the library's module.

const (
	maxConsumers    = 50
	maxCrossSymbols = 20
	maxCrossCallers = 10
)

// ExternalUse is a changed symbol and its callers in another repository.
type ExternalUse struct {
	Symbol string `json:"symbol"`
	Repo   string `json:"repo"`
	// Version is the library version that repository requires.
	Version string   `json:"version,omitempty"`
	Callers []string `json:"callers"`
	// More is set when there are callers beyond those listed.
	More bool `json:"more,omitempty"`
}

// crossRepo finds callers of the changed exported functions in consumers,
// each read through its own repository fence. all is the reader before the
// change's repository fence was applied.
func (s Service) crossRepo(ctx context.Context, all store.Reader, repo string, consumers []string, changed []model.Node) ([]ExternalUse, error) {
	if len(consumers) == 0 || len(changed) == 0 {
		return nil, nil
	}
	lib, err := s.R.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		return nil, err
	}
	module := model.GoModuleOf(lib)
	if module == "" {
		return nil, nil
	}
	changed = changed[:min(len(changed), maxCrossSymbols)]
	var out []ExternalUse
	for _, c := range consumers[:min(len(consumers), maxConsumers)] {
		if c == repo {
			continue
		}
		r := store.ForRepository(all, c)
		dep, err := r.Node(ctx, model.NodeID(c, model.KindModule, module))
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", c, err)
		}
		for _, n := range changed {
			ext, err := r.Node(ctx, model.NodeID(c, model.KindExternal, n.QualifiedName))
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			hits, err := r.Traverse(ctx, ext.ID, []model.EdgeKind{model.EdgeCalls}, store.In, 1, maxCrossCallers+1)
			if err != nil {
				return nil, err
			}
			u := ExternalUse{Symbol: n.ShortName(), Repo: c, Version: dep.Version, Callers: []string{}}
			for _, h := range hits[:min(len(hits), maxCrossCallers)] {
				u.Callers = append(u.Callers, fmt.Sprintf("%s %s:%d", h.Node.ShortName(), h.Node.File, h.Node.StartLine))
			}
			slices.Sort(u.Callers)
			u.More = len(hits) > maxCrossCallers
			if len(u.Callers) > 0 {
				out = append(out, u)
			}
		}
	}
	return out, nil
}
