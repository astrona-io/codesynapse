package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/astrona-io/codesynapse/internal/app"
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
