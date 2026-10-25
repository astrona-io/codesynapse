package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

const runsAnything = "package demo\n\nimport \"os/exec\"\n\n// Run starts what it is told to.\nfunc Run(name string) error { return exec.Command(name).Run() }\n"

// TestAuditChanged: only findings in staged files count, and a file with
// unstaged changes too is named, since the working tree is what is read.
func TestAuditChanged(t *testing.T) {
	ctx := context.Background()
	dir := gitRepo(t)
	audit := func(failOn string) (string, string, error) {
		t.Helper()
		var out, errs bytes.Buffer
		cmd := &cobra.Command{}
		cmd.Flags().Bool("vulns", true, "")
		cmd.Flags().Int("history", 500, "")
		o := &auditOptions{EC: &app.Env{Args: []string{dir}, Output: app.OutputMarkdown, Stdout: &out, Stderr: &errs, Command: cmd, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
			vulns: true, minSeverity: "low", failOn: failOn, changed: true}
		require.NoError(t, o.Complete(ctx, nil))
		require.NoError(t, o.Validate(ctx, nil))
		err := o.Run(ctx, nil)
		return out.String(), errs.String(), err
	}

	out, _, err := audit("medium")
	require.NoError(t, err)
	assert.Contains(t, out, "No staged files")

	write := func(name, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
	write("run.go", runsAnything)
	write("other.go", "package demo\n\nimport \"os/exec\"\n\nfunc Other(n string) error { return exec.Command(n).Run() }\n")
	write("demo.go", "package demo\n\n// Synced is in the server's graph.\nfunc Synced() int { return 2 }\n")
	_, err = gitx.Run(ctx, dir, "add", "run.go", "demo.go")
	require.NoError(t, err)
	write("demo.go", "package demo\n\n// Synced is in the server's graph.\nfunc Synced() int { return 3 }\n")

	out, errs, err := audit("medium")
	assert.True(t, errors.Is(err, errThreshold), "a medium finding in a staged file")
	assert.Contains(t, out, "Scope: 2 staged files")
	assert.Contains(t, out, "run.go:")
	assert.NotContains(t, out, "other.go", "not staged")
	assert.NotContains(t, out, "Grade", "the grade is the whole repository's")
	assert.Contains(t, errs, "demo.go also has unstaged changes")

	_, _, err = audit("high")
	assert.NoError(t, err, "nothing at high")
}

func TestPreCommitHook(t *testing.T) {
	ctx := context.Background()
	dir := gitRepo(t)
	ran := filepath.Join(t.TempDir(), "ran")
	fake := filepath.Join(t.TempDir(), "codesynapse")
	setExit := func(code string) {
		script := "#!/bin/sh\necho \"$@\" > '" + ran + "'\nexit " + code + "\n"
		require.NoError(t, os.WriteFile(fake, []byte(script), 0o700)) //nolint:gosec // a test executable
	}
	setExit("0")
	run := func(install bool) (string, error) {
		t.Helper()
		var buf bytes.Buffer
		o := &gitHookOptions{EC: &app.Env{Args: []string{dir}, Stdout: &buf, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
			install: install, preCommit: true, failOn: "critical", bin: fake}
		require.NoError(t, o.Complete(ctx, nil))
		if err := o.Validate(ctx, nil); err != nil {
			return "", err
		}
		err := o.Run(ctx, nil)
		return buf.String(), err
	}
	msg, err := run(true)
	require.NoError(t, err)
	assert.Contains(t, msg, "critical or above are refused")
	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")

	commitHook := func() error {
		cmd := exec.CommandContext(ctx, hook) //nolint:gosec // the hook just written
		cmd.Dir = dir
		return cmd.Run()
	}
	require.NoError(t, commitHook())
	b, err := os.ReadFile(ran) //nolint:gosec // test file
	require.NoError(t, err)
	assert.Equal(t, string(b), "audit "+dir+" --changed --fail-on critical\n")
	setExit("1")
	assert.Error(t, commitHook(), "a finding refuses the commit")

	msg, err = run(false)
	require.NoError(t, err)
	assert.Contains(t, msg, "removed")
	_, err = os.Stat(hook)
	assert.True(t, os.IsNotExist(err))

	o := &gitHookOptions{EC: &app.Env{Args: []string{dir}}, install: true, preCommit: true, failOn: "severe", bin: fake}
	require.NoError(t, o.Complete(ctx, nil))
	assert.Error(t, o.Validate(ctx, nil), "an unknown severity")
}
