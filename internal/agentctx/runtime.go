package agentctx

import (
	"context"
	"errors"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/runtimefacts"
	"github.com/astrona-io/codesynapse/internal/store"
)

// Measured is what the last recorded test run measured of a function
// (coverage profiles; internal/runtimefacts), next to the graph's static
// test reach.
type Measured struct {
	Coverage string `json:"coverage"`
	Percent  int    `json:"percent"`
	// At is the commit measured, or the time for local reports.
	At string `json:"at"`
	// Stale is set when the run measured another commit than the graph.
	Stale bool `json:"stale,omitempty"`
}

// runtimeFacts returns the repository's measured facts, if any, and
// whether they were measured at another commit than the indexed one.
func (s Service) runtimeFacts(ctx context.Context, repo string) (f runtimefacts.Facts, ok, stale bool, err error) {
	f, ok, err = store.RuntimeFactsOf(ctx, s.R, repo)
	if err != nil || !ok {
		return f, false, false, err
	}
	r, err := s.R.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return f, false, false, err
	}
	return f, true, f.StaleFor(r.Head), nil
}

func measuredOf(f runtimefacts.Facts, stale bool, id string) *Measured {
	note, ok := f.CoverageNote(id)
	if !ok {
		return nil
	}
	return &Measured{Coverage: note, Percent: f.Coverage[id].Percent(), At: f.At(), Stale: stale}
}

// annotateRuns notes flaky and failing tests on refs built from hits.
func annotateRuns(f runtimefacts.Facts, hits []model.Hit, refs []Ref) {
	for i := range refs {
		if note, ok := f.RunNote(hits[i].Node.ID); ok {
			refs[i].RunNote = note
		}
	}
}
