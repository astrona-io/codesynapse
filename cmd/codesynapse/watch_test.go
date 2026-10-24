package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestTreeKey(t *testing.T) {
	ctx := context.Background()
	dir := gitRepo(t)
	key := func() string {
		t.Helper()
		k, err := treeKey(ctx, dir)
		require.NoError(t, err)
		return k
	}
	clean := key()
	assert.Equal(t, key(), clean, "nothing changed")
	file := filepath.Join(dir, "demo.go")
	require.NoError(t, os.WriteFile(file, []byte("package demo\n\nfunc A() {}\n"), 0o600))
	edited := key()
	assert.True(t, edited != clean, "an edit")
	require.NoError(t, os.WriteFile(file, []byte("package demo\n\nfunc AB() {}\n"), 0o600))
	assert.True(t, key() != edited, "editing a file that was already modified")
	_, err := gitx.Run(ctx, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-am", "edit")
	require.NoError(t, err)
	committed := key()
	assert.True(t, committed != clean && committed != edited, "a commit moves HEAD")
	_, err = treeKey(ctx, t.TempDir())
	assert.True(t, err != nil, "not a git checkout")
}

// TestWatch: the graph follows an edit once it held for an interval.
func TestWatch(t *testing.T) {
	dir := gitRepo(t)
	mem := memory.New()
	o := &ingestOptions{EC: &app.Env{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, root: dir, repo: "demo", interval: 50 * time.Millisecond, watch: true, st: mem}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- o.watchLoop(ctx) }()
	has := func(name string) bool {
		_, err := mem.Node(context.Background(), model.NodeID("demo", model.KindFunction, "example.com/demo."+name))
		return err == nil
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for " + what)
			}
		}
	}
	waitFor("the first index", func() bool { return has("Synced") })
	require.NoError(t, os.WriteFile(filepath.Join(dir, "demo.go"), []byte("package demo\n\n// Synced is in the server's graph.\nfunc Synced() int { return 1 }\n\n// Edited after the watch started.\nfunc Edited() int { return 2 }\n"), 0o600))
	waitFor("the edit", func() bool { return has("Edited") })
	cancel()
	assert.NoError(t, <-done, "Ctrl-C ends the watch cleanly")
}

func TestGitHook(t *testing.T) {
	ctx := context.Background()
	dir := gitRepo(t)
	out := filepath.Join(t.TempDir(), "ran")
	fake := filepath.Join(t.TempDir(), "codesynapse")
	require.NoError(t, os.WriteFile(fake, []byte("#!/bin/sh\necho \"$@\" >> '"+out+"'\n"), 0o700)) //nolint:gosec // a test executable
	run := func(install bool, args ...string) (string, error) {
		t.Helper()
		var buf bytes.Buffer
		o := &gitHookOptions{EC: &app.Env{Args: append([]string{dir}, args...), Stdout: &buf, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, install: install, bin: fake}
		if install {
			o.repo = "acme/demo"
		}
		require.NoError(t, o.Complete(ctx, nil))
		if err := o.Validate(ctx, nil); err != nil {
			return "", err
		}
		err := o.Run(ctx, nil)
		return buf.String(), err
	}
	hook := filepath.Join(dir, ".git", "hooks", "post-commit")

	msg, err := run(true)
	require.NoError(t, err)
	assert.Contains(t, msg, "installed")
	st, err := os.Stat(hook)
	require.NoError(t, err)
	assert.True(t, st.Mode()&0o100 != 0, "executable")
	_, err = run(true)
	assert.NoError(t, err, "installing again replaces our own hook")

	// What git runs after a commit: the hook, from the checkout.
	cmd := exec.CommandContext(ctx, hook) //nolint:gosec // the hook just written
	cmd.Dir = dir
	require.NoError(t, cmd.Run())
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(out); err == nil { //nolint:gosec // test file
			assert.Equal(t, strings.TrimSpace(string(b)), "ingest "+dir+" --repo acme/demo")
			break
		}
		require.True(t, time.Now().Before(deadline), "the hook did not run ingest")
	}

	msg, err = run(false)
	require.NoError(t, err)
	assert.Contains(t, msg, "removed")
	require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\necho mine\n"), 0o700)) //nolint:gosec // a test hook
	_, err = run(true)
	assert.True(t, err != nil && strings.Contains(err.Error(), "not ours"), "someone else's hook is left alone")
	msg, err = run(false)
	require.NoError(t, err)
	assert.Contains(t, msg, "no CodeSynapse hook")
	b, err := os.ReadFile(hook) //nolint:gosec // test file
	require.NoError(t, err)
	assert.Equal(t, string(b), "#!/bin/sh\necho mine\n")

	// core.hooksPath in the repository's config is where git looks.
	_, err = gitx.Run(ctx, dir, "config", "core.hooksPath", ".githooks")
	require.NoError(t, err)
	_, err = run(true)
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, ".githooks", "post-commit"))
	assert.NoError(t, err)

	o := &gitHookOptions{EC: &app.Env{}, root: dir, repo: "it's", bin: fake}
	assert.True(t, o.Validate(ctx, nil) != nil, "a quote cannot go into the script")
}
