package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/model"
)

// AtCommit indexes the repository at root as it was at commit, in a
// temporary worktree that is removed afterwards; root itself is not
// touched. opts.Root is ignored. Used to compare a change with its base
// and to score the task eval against an older graph.
func AtCommit(ctx context.Context, root, commit string, opts Options) (*model.Graph, error) {
	if err := gitx.ValidateRev(commit); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "codesynapse-base-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp) //nolint:errcheck // temporary
	wt := filepath.Join(tmp, "tree")
	if err := gitx.AddWorktree(ctx, root, wt, commit); err != nil {
		return nil, fmt.Errorf("checking out %s: %w", commit, err)
	}
	defer gitx.RemoveWorktree(context.WithoutCancel(ctx), root, wt) //nolint:errcheck // best effort
	opts.Root = wt
	return Run(ctx, opts)
}
