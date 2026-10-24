package pgstore

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/accounts/accountstest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
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
	reset(ctx, t, st)
	accountstest.Run(t, st)
	accountstest.RunConcurrentAudit(t, st)
	accountstest.RunReBAC(t, st)
	accountstest.RunAdmin(t, st)
	accountstest.RunAccessBounds(t, st)
	accountstest.RunPolicyRecovery(t, st)
	accountstest.RunExecutionSessions(t, st)
	accountstest.RunRepoPolicy(t, st)
	accountstest.RunIngest(t, st)

	// A fresh connection pool resumes the same stream and obeys its owner fence.
	replica, err := Open(ctx, url)
	require.NoError(t, err)
	t.Cleanup(replica.Close)
	var before, after accounts.IngestMetricSnapshot
	require.NoError(t, st.ExportIngestTotals(ctx, func(ctx context.Context, snapshot accounts.IngestMetricSnapshot) error {
		before = snapshot
		err := replica.ExportIngestTotals(ctx, func(context.Context, accounts.IngestMetricSnapshot) error { return nil })
		assert.True(t, errors.Is(err, accounts.ErrIngestExporterBusy))
		return nil
	}))
	require.NoError(t, replica.ExportIngestTotals(ctx, func(_ context.Context, snapshot accounts.IngestMetricSnapshot) error { after = snapshot; return nil }))
	assert.Equal(t, after.StreamID, before.StreamID)
	assert.Equal(t, after.StartedAt, before.StartedAt)
	assert.Equal(t, after.Totals, before.Totals)
	assert.True(t, after.SampleAt.After(before.SampleAt))

	// Tampering with a stored entry is detected.
	_, err = st.db.Exec(ctx, "UPDATE events SET detail = 'edited' WHERE id = (SELECT max(id) FROM events)")
	require.NoError(t, err)
	v, err := accounts.VerifyAudit(ctx, st)
	require.NoError(t, err)
	assert.False(t, v.OK, "a changed entry breaks the chain")
}

func BenchmarkAccessOf(b *testing.B) {
	url := os.Getenv("CODESYNAPSE_TEST_DATABASE_URL")
	if url == "" {
		b.Skip("CODESYNAPSE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, url)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		b.Fatal(err)
	}
	userID, _ := accountstest.SeedAccessBenchmark(b, st, strconv.FormatInt(time.Now().UnixNano(), 10))
	svc := accounts.NewService(st, true)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := svc.AccessOf(ctx, userID); err != nil {
			b.Fatal(err)
		}
	}
}

// TestBootstrapContract gives each bootstrap scenario an empty database; it
// shares CODESYNAPSE_TEST_DATABASE_URL with TestContract, so packages run
// their tests one at a time.
func TestBootstrapContract(t *testing.T) {
	url := os.Getenv("CODESYNAPSE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CODESYNAPSE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	fresh := func(t *testing.T) accounts.Store {
		st, err := Open(ctx, url)
		require.NoError(t, err)
		t.Cleanup(st.Close)
		reset(ctx, t, st)
		require.NoError(t, st.Migrate(ctx))
		return st
	}
	accountstest.RunSSOStore(t, fresh)
	accountstest.RunJobs(t, fresh)
	accountstest.RunSchedules(t, fresh)
	accountstest.RunProfile(t, fresh)
	accountstest.RunBootstrap(t, func(t *testing.T) accounts.Store {
		st, err := Open(ctx, url)
		require.NoError(t, err)
		t.Cleanup(st.Close)
		reset(ctx, t, st)
		require.NoError(t, st.Migrate(ctx))
		return st
	})
}

// reset drops every table the migrations create.
func reset(ctx context.Context, t *testing.T, st *Store) {
	t.Helper()
	_, err := st.db.Exec(ctx, "DROP TABLE IF EXISTS notifications_seen, login_attempts, schedules, leases, jobs, idp_memberships, sso_states, external_identities, identity_providers, installation_setup, ingest_runs, ingest_repository_totals, ingest_metric_stream, repository_annotations, repository_policy_approvals, execution_sessions, access_policy_outbox, relation_tuples, teams, org_git_tokens, repositories, audit_head, events, api_keys, sessions, memberships, orgs, users, schema_migrations CASCADE")
	require.NoError(t, err)
}
