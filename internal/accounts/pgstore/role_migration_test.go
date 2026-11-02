package pgstore

import (
	"context"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestRoleNamesMigration renames every stored role and relation to the
// read/write/full names and leaves policy revisions alone, because nobody's
// access changes.
func TestRoleNamesMigration(t *testing.T) {
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
	_, err = tx.Exec(ctx, "CREATE SCHEMA role_migration_test; SET LOCAL search_path=role_migration_test")
	require.NoError(t, err)
	names, err := fs.Glob(migrations, "migrations/*.sql")
	require.NoError(t, err)
	slices.Sort(names)
	for _, name := range names {
		if strings.TrimPrefix(name, "migrations/") >= "0040" {
			break
		}
		raw, err := migrations.ReadFile(name)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, string(raw))
		require.NoError(t, err, name)
	}
	_, err = tx.Exec(ctx, `INSERT INTO users(id,email,name,password_hash,created_at) VALUES
 ('owner','owner@example.com','Owner','unused',now()),('ann','ann@example.com','Ann','unused',now()),
 ('bob','bob@example.com','Bob','unused',now()),('cat','cat@example.com','Cat','unused',now()),('guest','guest@example.com','Guest','unused',now());
 INSERT INTO orgs(id,slug,name,created_at) VALUES('org','org','Org',now());
 INSERT INTO memberships(org_id,user_id,role,added_at) VALUES('org','owner','owner',now()),('org','ann','admin',now()),
 ('org','bob','member',now()),('org','cat','viewer',now()),('org','guest','guest',now());
 INSERT INTO repositories(org_id,graph,visibility,added_by,added_at) VALUES('org','repo','public','owner',now());
 INSERT INTO teams(id,org_id,slug,name,created_at) VALUES('team','org','team','Team',now());
 INSERT INTO relation_tuples(object_type,object_id,relation,subject_type,subject_id,subject_relation,created_at) VALUES
 ('repository','org|repo','admin','user','ann','',now()),
 ('repository','org|repo','editor','team','team','member',now()),
 ('repository','org|repo','viewer','user','cat','',now()),
 ('repository','org|repo','none','user','guest','',now()),
 ('organisation','org','member','team','team','member',now()),
 ('team','team','maintainer','user','bob','',now()),
 ('team','team','member','user','cat','',now());
 INSERT INTO org_invitations(id,org_id,email,role,token_hash,invited_by,created_at,expires_at) VALUES
 ('i1','org','new@example.com','member','hash','owner',now(),now()+interval '1 day');
 INSERT INTO identity_providers(org_id,name,issuer,client_id,secret_preview,key_id,sealed_secret,settings,enabled,created_by,updated_at) VALUES
 ('org','IdP','https://idp.example.com','client','…','k','\x00','{"groups_claim":"groups","default_role":"viewer","role_groups":{"devs":"member","ops":"admin","boss":"owner"},"team_groups":{"devs":"team"}}',true,'owner',now());
 INSERT INTO platform_stats_daily(day,metric,dimension,org_id,value) VALUES(current_date,'memberships','member','org',1),(current_date,'memberships','owner','org',1);`)
	require.NoError(t, err)
	var before int64
	require.NoError(t, tx.QueryRow(ctx, "SELECT policy_revision FROM orgs WHERE id='org'").Scan(&before))

	raw, err := migrations.ReadFile("migrations/0040_role_names.sql")
	require.NoError(t, err)
	_, err = tx.Exec(ctx, string(raw))
	require.NoError(t, err)

	roles := map[string]string{}
	rows, err := tx.Query(ctx, "SELECT user_id, role FROM memberships")
	require.NoError(t, err)
	for rows.Next() {
		var u, r string
		require.NoError(t, rows.Scan(&u, &r))
		roles[u] = r
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, roles, map[string]string{"owner": "owner", "ann": "full", "bob": "write", "cat": "read", "guest": "guest"})

	var tuples []string
	rows, err = tx.Query(ctx, "SELECT object_type || ':' || relation || '@' || subject_id || '#' || subject_relation FROM relation_tuples ORDER BY 1")
	require.NoError(t, err)
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		tuples = append(tuples, s)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, tuples, []string{"organisation:write@team#read", "repository:deny@guest#", "repository:full@ann#", "repository:read@cat#", "repository:write@team#read", "team:full@bob#", "team:read@cat#"})

	var invite, settings, dim string
	require.NoError(t, tx.QueryRow(ctx, "SELECT role FROM org_invitations").Scan(&invite))
	assert.Equal(t, invite, "write")
	require.NoError(t, tx.QueryRow(ctx, "SELECT (settings->>'default_role') || ' ' || (settings->'role_groups')::text FROM identity_providers").Scan(&settings))
	assert.Equal(t, settings, `read {"ops": "full", "boss": "owner", "devs": "write"}`)
	require.NoError(t, tx.QueryRow(ctx, "SELECT string_agg(dimension, ',' ORDER BY dimension) FROM platform_stats_daily").Scan(&dim))
	assert.Equal(t, dim, "owner,write")

	var after int64
	require.NoError(t, tx.QueryRow(ctx, "SELECT policy_revision FROM orgs WHERE id='org'").Scan(&after))
	assert.Equal(t, after, before)
	_, err = tx.Exec(ctx, "INSERT INTO memberships(org_id,user_id,role,added_at) VALUES('org','owner','admin',now()) ON CONFLICT (org_id,user_id) DO UPDATE SET role='admin'")
	assert.Error(t, err)
}
