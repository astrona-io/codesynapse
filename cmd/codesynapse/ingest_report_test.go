package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/ingestreport"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestIngestReportBeforeExtractionFailure(t *testing.T) {
	output := filepath.Join(t.TempDir(), "report.json")
	env := &app.Env{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	options := &ingestOptions{EC: env, reportFile: output, graph: app.GraphFlags{Backend: "invalid"}}
	require.Error(t, options.Run(t.Context(), env))
	data, err := os.ReadFile(output)
	require.NoError(t, err)
	var report ingestreport.Report
	require.NoError(t, json.Unmarshal(data, &report))
	require.NoError(t, report.Validate())
	assert.Equal(t, report.Status, ingestreport.Failed)
	assert.Equal(t, report.GraphCommit, ingestreport.CommitNone)
	assert.Equal(t, report.TotalFilesScanned, 0)
	info, err := os.Stat(output)
	require.NoError(t, err)
	assert.Equal(t, info.Mode().Perm(), os.FileMode(0600))
	require.Error(t, options.Run(t.Context(), env))
	retained, err := os.ReadFile(output)
	require.NoError(t, err)
	assert.Equal(t, string(retained), string(data), "an existing report must not be replaced")
}

// --name-from-origin names the graph the way the dashboard names a
// repository added by URL, so adding that URL links the index.
func TestIngestNameFromOrigin(t *testing.T) {
	gitIn := func(dir string, args ...string) {
		t.Helper()
		_, err := gitx.Run(context.Background(), dir, args...)
		require.NoError(t, err)
	}
	for name, tc := range map[string]struct{ origin, want string }{
		"github https": {"https://github.com/Acme/Service.git", "acme/Service"},
		"github ssh":   {"git@github.com:acme/service.git", "acme/service"},
		"other host":   {"https://git.example.com/team/app.git", "git.example.com/team/app"},
	} {
		dir := t.TempDir()
		gitIn(dir, "init", "-q")
		gitIn(dir, "remote", "add", "origin", tc.origin)
		env := &app.Env{Args: []string{dir}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		o := &ingestOptions{EC: env, nameFromOrigin: true}
		require.NoError(t, o.Complete(context.Background(), env), name)
		assert.Equal(t, o.repo, tc.want, name)
	}

	// Without a usable origin the flag says so instead of guessing, and an
	// explicit --repo always wins.
	dir := t.TempDir()
	gitIn(dir, "init", "-q")
	env := &app.Env{Args: []string{dir}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	assert.Error(t, (&ingestOptions{EC: env, nameFromOrigin: true}).Complete(context.Background(), env))
	o := &ingestOptions{EC: env, nameFromOrigin: true, repo: "mine"}
	require.NoError(t, o.Complete(context.Background(), env))
	assert.Equal(t, o.repo, "mine")
	o = &ingestOptions{EC: env}
	require.NoError(t, o.Complete(context.Background(), env))
	assert.Equal(t, o.repo, filepath.Base(dir), "without the flag the directory name is still the default")
}
