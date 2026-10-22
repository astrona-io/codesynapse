package pgstore

import (
	"context"
	"os"
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/jackc/pgx/v5"
)

func TestAccessPolicyMigration(t *testing.T) {
	url := os.Getenv("CODESYNAPSE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CODESYNAPSE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, url)
	require.NoError(t, err)
	t.Cleanup(st.Close)
	tx, err := st.db.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }() // The test schema and fixtures never commit.
	_, err = tx.Exec(ctx, "CREATE SCHEMA policy_migration_test; SET LOCAL search_path=policy_migration_test")
	require.NoError(t, err)
	for _, name := range []string{"0001_accounts.sql", "0002_repositories.sql", "0003_git_tokens.sql", "0004_rebac.sql", "0005_admin.sql"} {
		raw, err := migrations.ReadFile("migrations/" + name)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, string(raw))
		require.NoError(t, err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO users(id,email,name,password_hash,created_at) VALUES('owner','owner@example.com','Owner','unused',now()),('external','external@example.com','External','unused',now());
 INSERT INTO orgs(id,slug,name,created_at) VALUES('org','org','Org',now());
 INSERT INTO memberships(org_id,user_id,role,added_at) VALUES('org','owner','owner',now());
 INSERT INTO repositories(org_id,graph,visibility,added_by,added_at) VALUES('org','repo','public','owner',now());
 INSERT INTO teams(id,org_id,slug,name,created_at) VALUES('team','org','team','Team',now());
 INSERT INTO relation_tuples(object_type,object_id,relation,subject_type,subject_id,subject_relation,created_at) VALUES
 ('repository','org|repo','admin','user','owner','',now()),
 ('repository','org|repo','viewer','user','owner','',now()),
 ('repository','org|repo','editor','team','team','member',now()),
 ('repository','org|repo','viewer','team','team','member',now()),
 ('repository','org|repo','viewer','user','external','',now());`)
	require.NoError(t, err)
	raw, err := migrations.ReadFile("migrations/0006_access_policy.sql")
	require.NoError(t, err)
	_, err = tx.Exec(ctx, string(raw))
	require.NoError(t, err)
	var direct, team, policy string
	require.NoError(t, tx.QueryRow(ctx, "SELECT relation FROM relation_tuples WHERE subject_id='owner'").Scan(&direct))
	assert.Equal(t, direct, accounts.RepoNone)
	require.NoError(t, tx.QueryRow(ctx, "SELECT relation FROM relation_tuples WHERE subject_id='team'").Scan(&team))
	assert.Equal(t, team, "invalid")
	require.NoError(t, tx.QueryRow(ctx, "SELECT default_repo_access FROM orgs WHERE id='org'").Scan(&policy))
	assert.Equal(t, policy, accounts.DefaultRepoNone)
	// Existing external shares remain reportable, but a new external grant fails.
	err = pgx.BeginFunc(ctx, tx, func(savepoint pgx.Tx) error {
		_, err := savepoint.Exec(ctx, "DELETE FROM relation_tuples WHERE subject_id='external'; INSERT INTO relation_tuples(object_type,object_id,relation,subject_type,subject_id,created_at) VALUES('repository','org|repo','viewer','user','external',now())")
		return err
	})
	assert.Error(t, err)
	var pending int
	require.NoError(t, tx.QueryRow(ctx, "SELECT count(*) FROM access_policy_outbox").Scan(&pending))
	assert.Equal(t, pending, 1)
}
