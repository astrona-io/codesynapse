package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/astrona-io/codesynapse/internal/gitx"
)

// ingest --watch (docs/PLAN.md, roadmap item 1): the graph follows the
// working tree. Publication is incremental, so a re-index after a one-file
// edit writes a few nodes. The tree is polled rather than watched: git
// already knows what changed, and a file watcher would be a dependency.

// treeKey fingerprints the working tree: HEAD and every changed file with
// its size and modification time. It changes on edits, commits, checkouts
// and resets.
func treeKey(ctx context.Context, root string) (string, error) {
	status, err := gitx.Run(ctx, root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00", gitx.HeadCommit(ctx, root)) //nolint:errcheck // hashes never fail
	hashStatus(h, root, status)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// watchLoop indexes once, then again each time the tree changed and held
// still for one interval (an editor saving several files, a rebase in
// progress). A failed run is logged and retried at the next change; the
// loop ends when ctx does (Ctrl-C).
func (o *ingestOptions) watchLoop(ctx context.Context) error {
	last, err := treeKey(ctx, o.root)
	if err != nil {
		return fmt.Errorf("--watch needs a git checkout: %w", err)
	}
	if err := o.once(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		o.EC.Logger.Warn("index failed; retrying at the next change", "repo", o.repo, "err", err)
	}
	o.EC.Logger.Info("watching for changes", "repo", o.repo, "root", o.root, "interval", o.interval)
	ticker := time.NewTicker(o.interval)
	defer ticker.Stop()
	pending := ""
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		key, err := treeKey(ctx, o.root)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil
			}
			o.EC.Logger.Warn("reading the working tree failed", "err", err)
		case key == last:
			pending = ""
		case key != pending:
			pending = key // changed: index once it holds for an interval
		default:
			start := time.Now()
			err := o.once(ctx)
			switch {
			case ctx.Err() != nil && errors.Is(err, ctx.Err()):
				return nil
			case err != nil:
				o.EC.Logger.Warn("index failed; retrying at the next change", "repo", o.repo, "err", err)
			default:
				o.EC.Logger.Info("re-indexed", "repo", o.repo, "took", time.Since(start).Round(time.Millisecond))
			}
			last, pending = key, ""
		}
	}
}
