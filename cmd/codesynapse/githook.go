package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/model"
)

// A git post-commit hook that re-indexes the repository after each commit
// (docs/PLAN.md, roadmap item 1), for people who run the CLI against their
// own NebulaGraph. It runs in the background so committing stays instant;
// commits made while a run is going are picked up by that run's next
// round. The store settings come from the committing shell's environment
// (CODESYNAPSE_NEBULA_*), never from the hook file.

// The markers identify hooks this command wrote; others are left alone.
const (
	gitHookMarker    = "# codesynapse post-commit hook"
	preCommitMarker  = "# codesynapse pre-commit hook"
	defaultPreCommit = "high"
)

type gitHookOptions struct {
	EC        *app.Env
	install   bool
	preCommit bool
	failOn    string
	repo      string
	root      string
	bin       string // the codesynapse binary the hook runs (tests replace it)
}

func (o *gitHookOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	f := o.EC.Command.Flags()
	f.BoolVar(&o.preCommit, "pre-commit", false, "The pre-commit security gate (audit --changed) instead of the post-commit re-index")
	if o.install {
		f.StringVar(&o.repo, "repo", "", "Repository name in the graph (default: directory name, as for ingest)")
		f.StringVar(&o.failOn, "fail-on", defaultPreCommit, "With --pre-commit: refuse a commit whose staged files have a finding at or above this severity")
	}
	return nil
}

// hook is the hook file name and the marker of the kind chosen.
func (o *gitHookOptions) hook() (name, marker string) {
	if o.preCommit {
		return "pre-commit", preCommitMarker
	}
	return "post-commit", gitHookMarker
}

func (o *gitHookOptions) Complete(_ context.Context, _ *app.Env) error {
	o.root = "."
	if len(o.EC.Args) > 0 {
		o.root = o.EC.Args[0]
	}
	abs, err := filepath.Abs(o.root)
	if err != nil {
		return err
	}
	o.root = abs
	if o.repo == "" {
		o.repo = filepath.Base(abs)
	}
	if o.bin == "" {
		if o.bin, err = os.Executable(); err != nil {
			return err
		}
	}
	return nil
}

func (o *gitHookOptions) Validate(_ context.Context, _ *app.Env) error {
	if o.install && o.preCommit && model.SeverityRank(o.failOn) < 0 {
		return fmt.Errorf("--fail-on must be one of %s", strings.Join(model.Severities, ", "))
	}
	// The values are quoted into a shell script: refuse what could end the
	// quoting rather than escaping it.
	for _, v := range []string{o.root, o.repo, o.bin} {
		if strings.ContainsAny(v, "'\n\r\x00") {
			return fmt.Errorf("%q cannot be written into a hook safely", v)
		}
	}
	return nil
}

func (o *gitHookOptions) Run(ctx context.Context, _ *app.Env) error {
	dir, err := hooksDir(ctx, o.root)
	if err != nil {
		return err
	}
	name, marker := o.hook()
	path := filepath.Join(dir, name)
	existing, err := os.ReadFile(path) //nolint:gosec // the repository's own hook file
	switch {
	case errors.Is(err, fs.ErrNotExist):
		existing = nil
	case err != nil:
		return err
	}
	ours := strings.Contains(string(existing), marker)
	if !o.install {
		if !ours {
			_, err := fmt.Fprintf(o.EC.Stdout, "no CodeSynapse hook in %s\n", path)
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		_, err := fmt.Fprintf(o.EC.Stdout, "removed %s\n", path)
		return err
	}
	if len(existing) > 0 && !ours {
		line := fmt.Sprintf("%s ingest '%s' --repo '%s' &", o.bin, o.root, o.repo)
		if o.preCommit {
			line = fmt.Sprintf("%s audit '%s' --changed --fail-on %s || exit 1", o.bin, o.root, o.failOn)
		}
		return fmt.Errorf("%s exists and is not ours; add this line to it instead:\n  %s", path, line)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	// Hooks must be executable to run.
	script := gitHookScript(o.bin, o.root, o.repo)
	if o.preCommit {
		script = preCommitScript(o.bin, o.root, o.failOn)
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // see above
		return err
	}
	if o.preCommit {
		_, err = fmt.Fprintf(o.EC.Stdout, "installed %s: commits whose staged files have a finding at %s or above are refused (git commit --no-verify skips it)\n", path, o.failOn)
		return err
	}
	_, err = fmt.Fprintf(o.EC.Stdout, "installed %s: each commit re-indexes %s in the background (log: codesynapse-ingest.log in the git directory)\n", path, o.repo)
	return err
}

// hooksDir is where git looks for this repository's hooks: core.hooksPath
// from the repository's own config, else hooks/ in the common git
// directory (shared by its worktrees). gitx.Run disables hooks for its own
// calls, so the setting is read from the repository config alone.
func hooksDir(ctx context.Context, root string) (string, error) {
	top, err := gitx.Run(ctx, root, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("not a git checkout: %w", err)
	}
	if out, err := gitx.Run(ctx, root, "config", "--local", "--get", "core.hooksPath"); err == nil {
		if p := strings.TrimSpace(string(out)); p != "" {
			if !filepath.IsAbs(p) {
				p = filepath.Join(strings.TrimSpace(string(top)), p)
			}
			return p, nil
		}
	}
	common, err := gitx.Run(ctx, root, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	dir := strings.TrimSpace(string(common))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	return filepath.Join(dir, "hooks"), nil
}

// gitHookScript re-indexes in the background. A lock directory keeps one
// run at a time; a pending mark makes the running one go again for commits
// that came in meanwhile, and a lock older than an hour (a run that was
// killed) is cleared. Rebases and cherry-picks commit many times: the hook
// waits for them to finish.
func gitHookScript(bin, root, repo string) string {
	return `#!/bin/sh
` + gitHookMarker + `: re-index the repository after each commit.
# Installed by "codesynapse git-hook install"; remove with "codesynapse git-hook uninstall".
git_dir=$(git rev-parse --git-common-dir) || exit 0
[ -d "$(git rev-parse --git-path rebase-merge)" ] && exit 0
[ -d "$(git rev-parse --git-path rebase-apply)" ] && exit 0
bin='` + bin + `'
[ -x "$bin" ] || bin=$(command -v codesynapse) || exit 0
lock="$git_dir/codesynapse-ingest.lock"
pending="$git_dir/codesynapse-ingest.pending"
log="$git_dir/codesynapse-ingest.log"
touch "$pending"
find "$lock" -maxdepth 0 -mmin +60 -exec rmdir {} \; 2>/dev/null
mkdir "$lock" 2>/dev/null || exit 0
(
	trap 'rmdir "$lock"' EXIT
	while [ -e "$pending" ]; do
		rm -f "$pending"
		"$bin" ingest '` + root + `' --repo '` + repo + `' >>"$log" 2>&1
	done
) >/dev/null 2>&1 &
exit 0
`
}

// preCommitScript checks the staged files and refuses the commit on a
// finding at failOn or above. Without the binary the commit goes ahead: a
// missing tool must not block work. failOn is one of model.Severities.
func preCommitScript(bin, root, failOn string) string {
	return `#!/bin/sh
` + preCommitMarker + `: refuse commits that add security findings.
# Installed by "codesynapse git-hook install --pre-commit"; remove with "codesynapse git-hook uninstall --pre-commit".
# Skip it once with: git commit --no-verify
bin='` + bin + `'
[ -x "$bin" ] || bin=$(command -v codesynapse) || exit 0
exec "$bin" audit '` + root + `' --changed --fail-on ` + failOn + `
`
}

func gitHookCmd(ec *app.Env) *cobra.Command {
	parent := &cobra.Command{Use: "git-hook", Short: "Re-index the repository after each commit", GroupID: app.GroupBase}
	parent.AddCommand(app.NewSubCommand("install [path]", &gitHookOptions{EC: ec, install: true}, ec).
		WithShortDesc("Install a git post-commit hook that re-indexes the repository in the background").
		WithLongDesc("Writes .git/hooks/post-commit (or into core.hooksPath) so every commit runs `codesynapse ingest` in the background; publication writes only what changed. The store settings come from the environment of the shell that commits (CODESYNAPSE_NEBULA_ADDRS, _USER, _PASSWORD), never from the hook. A hook that is not CodeSynapse's is left alone. Output goes to codesynapse-ingest.log in the git directory. With --pre-commit it writes .git/hooks/pre-commit instead, which runs `codesynapse audit --changed` on the staged files and refuses the commit on a finding at --fail-on or above (offline; `git commit --no-verify` skips it once).").
		WithExample("  codesynapse git-hook install . --repo my-service\n  codesynapse git-hook install . --pre-commit --fail-on high\n  codesynapse git-hook uninstall .").
		WithMaxArgs(1).Build())
	parent.AddCommand(app.NewSubCommand("uninstall [path]", &gitHookOptions{EC: ec}, ec).
		WithShortDesc("Remove the CodeSynapse post-commit hook (or, with --pre-commit, the pre-commit gate)").
		WithMaxArgs(1).Build())
	return parent
}
