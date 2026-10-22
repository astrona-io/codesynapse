package memstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/accounts"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

var errAuditUnavailable = errors.New("audit unavailable")

type failedAuditStore struct{ accounts.Store }

func (s failedAuditStore) AddEvent(context.Context, accounts.Event) (accounts.Event, error) {
	return accounts.Event{}, errAuditUnavailable
}
func (s failedAuditStore) PolicyTransaction(ctx context.Context, orgID string, fn func(accounts.Store) error) error {
	return s.Store.PolicyTransaction(ctx, orgID, func(tx accounts.Store) error { return fn(failedAuditStore{tx}) })
}

func TestPolicyAuditFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	st := New()
	now := time.Now().UTC()
	u := accounts.User{ID: "owner", Email: "owner@example.com", CreatedAt: now}
	require.NoError(t, st.CreateUser(ctx, u))
	require.NoError(t, st.CreateOrg(ctx, accounts.Org{ID: "org", Slug: "org", CreatedAt: now}, u.ID))
	svc := accounts.NewService(failedAuditStore{st}, true)
	before, err := st.OrgByID(ctx, "org")
	require.NoError(t, err)
	err = svc.SetDefaultRepoAccess(ctx, accounts.Principal{User: u, Via: "session"}, "org", accounts.DefaultRepoReadOnly)
	assert.True(t, errors.Is(err, errAuditUnavailable))
	after, err := st.OrgByID(ctx, "org")
	require.NoError(t, err)
	assert.Equal(t, after, before)
}

type unavailableTeamStore struct{ accounts.Store }

func (s unavailableTeamStore) ReadTuples(context.Context, accounts.TupleFilter) ([]accounts.Tuple, error) {
	return nil, accounts.ErrAccessLimit
}

func TestOwnerControlDoesNotNeedTeamEvaluation(t *testing.T) {
	ctx := context.Background()
	st := New()
	u := accounts.User{ID: "owner", Email: "owner@example.com", CreatedAt: time.Now().UTC()}
	require.NoError(t, st.CreateUser(ctx, u))
	require.NoError(t, st.CreateOrg(ctx, accounts.Org{ID: "org", Slug: "org", CreatedAt: u.CreatedAt}, u.ID))
	svc := accounts.NewService(unavailableTeamStore{st}, true)
	require.NoError(t, svc.Require(ctx, accounts.Principal{User: u, Via: "session"}, "org", accounts.Owner))
}
