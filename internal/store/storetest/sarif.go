package storetest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/sarifimport"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// sarifContract: imported findings round-trip per namespace, beside and
// apart from the runtime facts of the same namespace, organisations sharing
// a graph never see each other's, and deleting the graph removes them.
func sarifContract(t *testing.T, s interface {
	store.Reader
	store.Writer
	store.RuntimeStore
	store.SARIFStore
}) {
	ctx := context.Background()
	const graph = "sarif-graph"
	acme, globex := "org-acme|"+graph, "org-globex|"+graph

	_, err := s.SARIFFindings(ctx, acme)
	assert.True(t, errors.Is(err, store.ErrNotFound))

	run := sarifimport.StoredRun{Run: sarifimport.Run{Tool: "gitleaks", Version: "8.21.2"}, Commit: "abc1234",
		UploadedAt: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)}
	for i := range 5000 {
		run.Findings = append(run.Findings, sarifimport.Finding{RuleID: "aws-access-token", Severity: "high", Level: "error",
			File: fmt.Sprintf("config/f%d.yaml", i), StartLine: i + 1, EndLine: i + 1, Message: "AWS token found", Fingerprint: fmt.Sprintf("%024x", i)})
	}
	require.NoError(t, s.SetSARIFFindings(ctx, acme, sarifimport.Set{}.With([]sarifimport.StoredRun{run})))
	got, err := s.SARIFFindings(ctx, acme)
	require.NoError(t, err)
	require.Equal(t, len(got.Runs), 1)
	assert.Equal(t, got.Runs[0].Tool, "gitleaks")
	assert.Equal(t, got.Runs[0].Version, "8.21.2")
	assert.Equal(t, len(got.Runs[0].Findings), 5000)
	assert.Equal(t, got.Runs[0].Findings[42].StartLine, 43)
	assert.True(t, got.Runs[0].UploadedAt.Equal(run.UploadedAt))

	_, err = s.RuntimeFacts(ctx, acme)
	assert.True(t, errors.Is(err, store.ErrNotFound), "findings are not runtime facts")
	_, err = s.SARIFFindings(ctx, globex)
	assert.True(t, errors.Is(err, store.ErrNotFound), "another organisation on the same graph sees nothing")

	small := sarifimport.Set{Runs: []sarifimport.StoredRun{{Run: sarifimport.Run{Tool: "zizmor"}, Commit: "def5678"}}}
	require.NoError(t, s.SetSARIFFindings(ctx, acme, small))
	got, err = s.SARIFFindings(ctx, acme)
	require.NoError(t, err)
	require.Equal(t, len(got.Runs), 1, "a write replaces the namespace's set")
	assert.Equal(t, got.Runs[0].Tool, "zizmor")

	require.NoError(t, s.SetSARIFFindings(ctx, globex, small))
	require.NoError(t, s.DeleteSARIFFindings(ctx, globex))
	_, err = s.SARIFFindings(ctx, globex)
	assert.True(t, errors.Is(err, store.ErrNotFound))

	require.NoError(t, s.SetSARIFFindings(ctx, globex, small))
	require.NoError(t, s.DeleteRepo(ctx, graph))
	for _, ns := range []string{acme, globex} {
		_, err = s.SARIFFindings(ctx, ns)
		assert.True(t, errors.Is(err, store.ErrNotFound), "deleting the graph removes "+ns)
	}
}
