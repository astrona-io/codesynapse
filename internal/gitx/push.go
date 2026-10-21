package gitx

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// FixBranchPrefix is the only branch namespace CodeSynapse pushes to.
const FixBranchPrefix = "codesynapse/"

// AddWorktree checks out commit, detached, into path. Fixes are made in a
// separate worktree so the indexed checkout never changes.
func AddWorktree(ctx context.Context, dir, path, commit string) error {
	if err := ValidateRev(commit); err != nil {
		return err
	}
	_, err := Run(ctx, dir, "worktree", "add", "--force", "--detach", "--", path, commit)
	return err
}

// RemoveWorktree deletes a worktree made by AddWorktree.
func RemoveWorktree(ctx context.Context, dir, path string) error {
	_, err := Run(ctx, dir, "worktree", "remove", "--force", "--", path)
	if err != nil {
		_, _ = Run(ctx, dir, "worktree", "prune") //nolint:errcheck // best effort
	}
	return err
}

// Author identifies the committer of generated fixes.
type Author struct{ Name, Email string }

// CommitFiles stages exactly files and commits them in worktree wt, returning
// the new commit id. Hooks are disabled by Run's hardening.
func CommitFiles(ctx context.Context, wt string, files []string, message string, a Author) (string, error) {
	if len(files) == 0 {
		return "", errors.New("nothing to commit")
	}
	for _, f := range files {
		if f == "" || strings.HasPrefix(f, "-") || strings.HasPrefix(f, "/") || strings.Contains(f, "..") {
			return "", fmt.Errorf("refusing to commit %q", f)
		}
	}
	if _, err := Run(ctx, wt, append([]string{"add", "--"}, files...)...); err != nil {
		return "", err
	}
	if _, err := Run(ctx, wt, "-c", "user.name="+a.Name, "-c", "user.email="+a.Email, "-c", "commit.gpgsign=false",
		"commit", "--no-verify", "-q", "-m", message); err != nil {
		return "", err
	}
	out, err := Run(ctx, wt, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// PushBranch pushes HEAD of wt to a new branch on remote. Only branches under
// FixBranchPrefix are allowed and nothing is ever force-pushed, so an
// existing branch (including the default branch) can never be overwritten.
func PushBranch(ctx context.Context, wt, remote, branch string, auth Auth) error {
	if !strings.HasPrefix(branch, FixBranchPrefix) {
		return fmt.Errorf("refusing to push %q: fix branches must start with %s", branch, FixBranchPrefix)
	}
	if err := ValidateRev(branch); err != nil {
		return err
	}
	if u, err := url.Parse(remote); err != nil || u.Scheme != "https" {
		return fmt.Errorf("%w: push needs an https remote", ErrInvalidRemote)
	}
	_, err := runRemote(ctx, wt, remote, auth, "push", "--no-verify", "--", remote, "HEAD:refs/heads/"+branch)
	return err
}
