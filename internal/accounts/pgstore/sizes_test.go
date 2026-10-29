package pgstore

import (
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestSizedTablesExist: every sampled table is one a migration creates, and
// the statements carry no value in their text.
func TestSizedTablesExist(t *testing.T) {
	created := map[string]bool{}
	re := regexp.MustCompile(`(?i)CREATE TABLE (?:IF NOT EXISTS )?([a-z_]+)`)
	require.NoError(t, fs.WalkDir(migrations, "migrations", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := migrations.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			created[m[1]] = true
		}
		return nil
	}))
	for _, table := range accounts.SizedTables {
		assert.True(t, created[table], "no migration creates %s", table)
		assert.True(t, telemetry.StoreKindAllowed(telemetry.DBSystemPostgres, table))
	}
	for _, stmt := range []string{sizeOfDatabase, sizeOfTables} {
		assert.False(t, strings.Contains(stmt, "%"), "format verb in %q", stmt)
	}
}

func TestStoreSizes(t *testing.T) {
	url := os.Getenv("CODESYNAPSE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CODESYNAPSE_TEST_DATABASE_URL not set")
	}
	st, err := Open(t.Context(), url)
	require.NoError(t, err)
	t.Cleanup(st.Close)
	sizes, err := st.StoreSizes(t.Context())
	require.NoError(t, err)
	require.True(t, len(sizes) > 1)
	assert.Equal(t, sizes[0].Kind, accounts.SizeKindDatabase)
	assert.True(t, sizes[0].Bytes > 0)
	seen := map[string]bool{}
	for _, s := range sizes {
		assert.True(t, telemetry.StoreKindAllowed(s.System, s.Kind), "kind %s", s.Kind)
		seen[s.Kind] = true
	}
	assert.True(t, seen["events"], "the audit log is sized")
}
