// Package gitx runs git against untrusted repositories and parses its output.
//
// A cloned repository can carry configuration that makes ordinary git
// commands execute code (core.fsmonitor, external diff drivers, textconv
// filters). Every invocation here disables those hooks, never goes through a
// shell, and validates user-supplied revisions before passing them on.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/astrona-io/codesynapse/internal/execution"
)

// ErrInvalidRev is returned for revisions that could be parsed as options or
// contain characters git does not allow in ref names.
var ErrInvalidRev = errors.New("invalid git revision")

var revPattern = regexp.MustCompile(`^[A-Za-z0-9._/@^~{}-]+$`)

// ValidateRev rejects revisions that could be interpreted as command-line
// options or that contain unexpected characters.
func ValidateRev(rev string) error {
	if rev == "" || strings.HasPrefix(rev, "-") || strings.Contains(rev, "..") || !revPattern.MatchString(rev) {
		return fmt.Errorf("%w: %q", ErrInvalidRev, rev)
	}
	return nil
}

// hardening is prepended to every git invocation.
var hardening = []string{
	"-c", "core.fsmonitor=false",
	"-c", "core.hooksPath=/dev/null",
	"-c", "diff.external=",
	"-c", "protocol.allow=never",
}

// Run executes git in dir with args and returns stdout.
func Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	// safe.directory lets git read bind-mounted repos owned by another uid
	// (the Docker case); it is scoped to exactly this directory.
	full := append([]string{"-C", dir, "-c", "safe.directory=" + dir}, hardening...)
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, "git", full...) //nolint:gosec // fixed binary, no shell; revisions validated by callers
	cmd.Env = []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"HOME=/nonexistent", // ignore the user's global config and credential helpers
		"PATH=" + safePath(),
	}
	var err error
	cmd.Env, err = execution.Environment(ctx, cmd.Env)
	if err != nil {
		return nil, err
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func safePath() string {
	return "/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin"
}

// Diff returns the unified diff between base and head (merge-base semantics,
// like a pull request). Zero context lines keep hunks tight around changes.
func Diff(ctx context.Context, dir, base, head string) ([]byte, error) {
	if err := ValidateRev(base); err != nil {
		return nil, err
	}
	if err := ValidateRev(head); err != nil {
		return nil, err
	}
	return Run(ctx, dir, "diff", "--no-color", "--no-ext-diff", "--no-textconv",
		"--unified=0", base+"..."+head, "--")
}

// HeadCommit returns the full hash of HEAD, or "" when dir is not a repository.
func HeadCommit(ctx context.Context, dir string) string {
	out, err := Run(ctx, dir, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
