package pgstore

import (
	"context"
	"os"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/accounts"
	"github.com/parisnakitakejser/codesynapse/internal/accounts/accountstest"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

// TestContract runs against an empty PostgreSQL database named by
// CODESYNAPSE_TEST_DATABASE_URL (just test-integration starts one).
func TestContract(t *testing.T) {
	url := os.Getenv("CODESYNAPSE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CODESYNAPSE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, url)
	require.NoError(t, err)
	t.Cleanup(st.Close)
	// Start from an empty schema: the contract creates fixed emails.
	_, err = st.db.Exec(ctx, "DROP TABLE IF EXISTS relation_tuples, teams, org_git_tokens, repositories, audit_head, events, api_keys, sessions, memberships, orgs, users, schema_migrations CASCADE")
	require.NoError(t, err)
	accountstest.Run(t, st)
	accountstest.RunConcurrentAudit(t, st)
	accountstest.RunReBAC(t, st)

	// Tampering with a stored entry is detected.
	_, err = st.db.Exec(ctx, "UPDATE events SET detail = 'edited' WHERE id = (SELECT max(id) FROM events)")
	require.NoError(t, err)
	v, err := accounts.VerifyAudit(ctx, st)
	require.NoError(t, err)
	assert.False(t, v.OK, "a changed entry breaks the chain")
}
