package store

import (
	"context"
	"errors"

	"github.com/astrona-io/codesynapse/internal/runtimefacts"
)

// RuntimeReader is implemented by stores that keep measured runtime facts
// (coverage, test results; internal/runtimefacts) per repository, apart
// from the code graph so a re-index keeps them.
type RuntimeReader interface {
	// RuntimeFacts returns ErrNotFound when the repository has none.
	RuntimeFacts(ctx context.Context, repo string) (runtimefacts.Facts, error)
}

// RuntimeFactsOf returns repo's measured facts; ok is false when r keeps
// none for it or keeps none at all.
func RuntimeFactsOf(ctx context.Context, r Reader, repo string) (facts runtimefacts.Facts, ok bool, err error) {
	rr, isRuntime := r.(RuntimeReader)
	if !isRuntime {
		return runtimefacts.Facts{}, false, nil
	}
	facts, err = rr.RuntimeFacts(ctx, repo)
	if errors.Is(err, ErrNotFound) {
		return runtimefacts.Facts{}, false, nil
	}
	return facts, err == nil, err
}

// RuntimeFacts keeps the repository fence: another repository's facts are
// not found.
func (s repositoryReader) RuntimeFacts(ctx context.Context, repo string) (runtimefacts.Facts, error) {
	rr, ok := s.reader.(RuntimeReader)
	if !ok || !s.allows(repo) {
		return runtimefacts.Facts{}, ErrNotFound
	}
	return rr.RuntimeFacts(ctx, repo)
}
