package agentctx

import (
	"context"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/runtimefacts"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestMeasuredFacts(t *testing.T) {
	ctx := context.Background()
	s := svc(t)

	// Without recorded facts, answers carry none.
	sc, err := s.SymbolContext(ctx, storetest.Repo, "app.Handler", ContextOptions{})
	require.NoError(t, err)
	assert.True(t, sc.Measured == nil)

	handler, test := storetest.ID(model.KindFunction, "app.Handler"), storetest.ID(model.KindFunction, "app.TestHandler")
	mem, ok := s.R.(*memory.Store)
	require.True(t, ok)
	require.NoError(t, mem.SetRuntimeFacts(ctx, storetest.Repo, runtimefacts.Facts{
		Commit: "0123456789abcdef", Unit: "statements",
		Coverage: map[string]runtimefacts.Coverage{handler: {Covered: 3, Total: 4}},
		Tests:    map[string]runtimefacts.TestRun{test: {Passed: 1, Failed: 1}},
	}))
	// Another repository's facts stay behind the repository fence.
	require.NoError(t, mem.SetRuntimeFacts(ctx, "other", runtimefacts.Facts{Coverage: map[string]runtimefacts.Coverage{handler: {Covered: 0, Total: 4}}}))

	sc, err = s.SymbolContext(ctx, storetest.Repo, "app.Handler", ContextOptions{})
	require.NoError(t, err)
	require.NotNil(t, sc.Measured)
	assert.Equal(t, sc.Measured.Coverage, "3 of 4 statements covered (75%)")
	assert.Equal(t, sc.Measured.At, "0123456789ab")
	require.NotEmpty(t, sc.Tests)
	assert.True(t, strings.HasPrefix(sc.Tests[0].RunNote, "flaky"), sc.Tests[0].RunNote)

	pr, err := s.PRContext(ctx, storetest.Repo, PROptions{Files: []string{"app/app.go"}})
	require.NoError(t, err)
	require.NotNil(t, pr.Measured)
	assert.Contains(t, strings.Join(pr.Measured.Coverage, "\n"), "Handler: 3 of 4 statements covered (75%)")
	require.Len(t, pr.Measured.Tests, 1)
	assert.Contains(t, pr.Measured.Tests[0], "flaky")
}
