package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/ingest"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestPublicationSerializesSharedGraphAttachment(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	ctx := t.Context()
	principal := accounts.Principal{User: f.user, Via: "session"}
	other, err := f.accounts.CreateOrg(ctx, principal, "Concurrent graph owner", "")
	require.NoError(t, err)
	policy, err := repoconfig.Parse([]byte("version: 1\nignore:\n  paths: [generated/**]\n"))
	require.NoError(t, err)
	canonical, err := json.Marshal(policy.Config())
	require.NoError(t, err)
	require.NoError(t, f.accounts.SetRepoPolicy(ctx, principal, accounts.RepoPolicyApproval{OrgID: f.org.ID, Graph: storetest.Repo, Digest: policy.Digest(), ConfigJSON: string(canonical), Reason: "Reviewed exclusions", AllowExclusions: true}))
	graph := storetest.Fixture()
	repo, ok := graph.Node(model.NodeID(storetest.Repo, model.KindRepository, storetest.Repo))
	require.True(t, ok)
	meta, err := json.Marshal(map[string]any{"repository_policy": ingest.PolicyMetadata{Present: true, Digest: policy.Digest(), Config: policy.Config(), ExclusionsApproved: true}})
	require.NoError(t, err)
	repo.Meta = string(meta)
	workCtx, err := execution.With(ctx, f.identity)
	require.NoError(t, err)
	otherID, err := f.accounts.RegisterExecutionSession(ctx, f.user.ID, other.ID, "parallel-link-session")
	require.NoError(t, err)
	linkCtx, err := execution.With(context.WithValue(ctx, principalKey{}, principal), otherID)
	require.NoError(t, err)
	locked, release := make(chan struct{}), make(chan struct{})
	published := make(chan error, 1)
	go func() {
		published <- f.server.publishRepo(workCtx, storetest.Repo, func(txCtx context.Context) error {
			if _, err := f.server.approveIngestPolicy(txCtx, storetest.Repo, policy); err != nil {
				return err
			}
			close(locked)
			<-release
			txCtx = store.WithCommitCheck(txCtx, func(checkCtx context.Context) error {
				_, err := f.server.approveIngestPolicy(checkCtx, storetest.Repo, policy)
				return err
			})
			return f.store.ReplaceRepo(txCtx, storetest.Repo, graph)
		})
	}()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not acquire policy lock")
	}
	linked := make(chan error, 1)
	go func() {
		linked <- f.server.linkPolicyRepository(linkCtx, accounts.Repository{OrgID: other.ID, Graph: storetest.Repo, AddedBy: f.user.ID, Visibility: accounts.Private, AddedAt: time.Now()}, accounts.Member)
	}()
	select {
	case err := <-linked:
		t.Fatalf("attachment bypassed publication lock: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-published:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("publication callback deadlocked")
	}
	select {
	case err := <-linked:
		assert.True(t, errors.Is(err, accounts.ErrForbidden))
	case <-time.After(5 * time.Second):
		t.Fatal("attachment did not resume")
	}
}

func TestPublicationDenialAuditSurvivesRollback(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	ctx, err := execution.With(t.Context(), f.identity)
	require.NoError(t, err)
	err = f.server.publishRepo(ctx, storetest.Repo, func(txCtx context.Context) error {
		f.server.recordJobDenial(txCtx, accounts.Event{OrgID: f.org.ID, UserID: f.user.ID, Action: "job.authorization_denied", Target: storetest.Repo, Outcome: accounts.Failure})
		return accounts.ErrForbidden
	})
	assert.True(t, errors.Is(err, accounts.ErrForbidden))
	rows, err := f.accounts.St.ListEvents(ctx, accounts.EventFilter{OrgID: f.org.ID, Action: "job.authorization_denied", Limit: 10})
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}
