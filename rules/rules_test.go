package rules

import (
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestCatalog(t *testing.T) {
	require.NoError(t, Err())
	all := All()
	require.Greater(t, len(all), 30)
	assert.Equal(t, all[0].Severity, "critical", "most severe first")
	r, ok := Get("sql-concat")
	require.True(t, ok)
	assert.Equal(t, r.File, "rules/code/sql-concat.yaml")
	assert.Contains(t, r.CWE, "CWE-89")
}

func TestValidate(t *testing.T) {
	ok := Rule{ID: "x", Title: "t", Severity: "high", Category: "code", Why: "w", Fix: "f", Examples: []Example{{"go", "a", "b"}}}
	assert.NoError(t, validate(ok, "code/x.yaml"))
	bad := ok
	bad.ID = "y"
	assert.Error(t, validate(bad, "code/x.yaml"), "id must match file")
	bad = ok
	bad.Severity = "urgent"
	assert.Error(t, validate(bad, "code/x.yaml"))
	bad = ok
	bad.Category = "config"
	assert.Error(t, validate(bad, "code/x.yaml"), "category must match directory")
	bad = ok
	bad.Examples = nil
	assert.Error(t, validate(bad, "code/x.yaml"))
}
