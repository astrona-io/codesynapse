package accounts_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// fakeDecider answers from fixed decisions and records what it was asked.
type fakeDecider struct {
	orgs  map[string]accounts.OrgDecision
	repos map[string]accounts.Perms
	err   error
	asked []accounts.DecisionRequest
}

func (f *fakeDecider) Decide(_ context.Context, q accounts.DecisionRequest) (accounts.Decision, error) {
	f.asked = append(f.asked, q)
	if f.err != nil {
		return accounts.Decision{}, f.err
	}
	d := accounts.Decision{Orgs: map[string]accounts.OrgDecision{}, Repos: map[string]accounts.Perms{}}
	for _, o := range q.Orgs {
		if od, ok := f.orgs[o.ID]; ok {
			d.Orgs[o.ID] = od
		}
	}
	for _, r := range q.Repos {
		key := accounts.RepoObject(r.OrgID, r.Graph)
		if p, ok := f.repos[key]; ok {
			d.Repos[key] = p
		}
	}
	return d, nil
}

func deciderService(t *testing.T) (*accounts.Service, *memstore.Store, *fakeDecider) {
	t.Helper()
	ctx := t.Context()
	st := memstore.New()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	require.NoError(t, st.CreateUser(ctx, accounts.User{ID: "u1", Name: "u1", Email: "u1@example.com", CreatedAt: now}))
	for _, id := range []string{"o1", "o2"} {
		require.NoError(t, st.CreateOrg(ctx, accounts.Org{ID: id, Name: id, Slug: id, CreatedAt: now}, "u1"))
		require.NoError(t, st.AddRepository(ctx, accounts.Repository{OrgID: id, Graph: "g/a", AddedBy: "u1", AddedAt: now}))
	}
	f := &fakeDecider{
		orgs: map[string]accounts.OrgDecision{
			"o1": {Active: true, Role: accounts.Write, Perms: accounts.Write.Bundle(accounts.ResourceOrganisation)},
			"o2": {Active: true, Role: accounts.Read, Perms: accounts.Read.Bundle(accounts.ResourceOrganisation)},
		},
		repos: map[string]accounts.Perms{
			accounts.RepoObject("o1", "g/a"): accounts.Read.Bundle(accounts.ResourceRepository),
			// OpenFGA holds a grant on an entry PostgreSQL does not have.
			accounts.RepoObject("o1", "g/gone"): accounts.Full.Bundle(accounts.ResourceRepository),
		},
	}
	s := accounts.NewService(st, true)
	s.Decider = f
	return s, st, f
}

func TestDeciderDecides(t *testing.T) {
	ctx := t.Context()
	s, _, f := deciderService(t)
	a, err := s.AccessOf(ctx, "u1")
	require.NoError(t, err)
	// PostgreSQL makes u1 an owner of both; the decision service says otherwise.
	assert.Equal(t, a.Orgs, map[string]accounts.Role{"o1": accounts.Write, "o2": accounts.Read})
	require.Len(t, a.Repos, 1)
	assert.Equal(t, a.Repos[0].Graph, "g/a")
	assert.Equal(t, a.Repos[0].OrgID, "o1")
	assert.Equal(t, a.Repos[0].Role, accounts.Read)
	require.Len(t, f.asked, 1)
	assert.Len(t, f.asked[0].Repos, 2, "only entries PostgreSQL has")

	p := accounts.Principal{User: accounts.User{ID: "u1"}, Via: "session"}
	r, err := s.RoleIn(ctx, p, "o1")
	require.NoError(t, err)
	assert.Equal(t, r, accounts.Write)
	assert.NoError(t, s.Require(ctx, p, "o1", accounts.OrgMembership))
	assert.True(t, errors.Is(s.Require(ctx, p, "o1", accounts.OrgOwnerRules), accounts.ErrForbidden), "the decision, not the owner membership, answers")
	perms, err := s.PermsIn(ctx, p, "o2")
	require.NoError(t, err)
	assert.Equal(t, perms, accounts.Read.Bundle(accounts.ResourceOrganisation))
}

func TestDeciderRefuses(t *testing.T) {
	p := accounts.Principal{User: accounts.User{ID: "u1"}, Via: "session"}
	t.Run("a stale organisation asked about alone", func(t *testing.T) {
		ctx := t.Context()
		s, _, f := deciderService(t)
		delete(f.orgs, "o1")
		_, err := s.AccessIn(ctx, p, accounts.AccessQuery{OrgID: "o1"})
		assert.True(t, errors.Is(err, accounts.ErrPolicyStale), "%v", err)
		_, err = s.RoleIn(ctx, p, "o1")
		assert.True(t, errors.Is(err, accounts.ErrPolicyStale), "%v", err)
		assert.True(t, errors.Is(s.Require(ctx, p, "o1", accounts.OrgMembership), accounts.ErrPolicyStale))
	})
	t.Run("a stale organisation among others is left out", func(t *testing.T) {
		ctx := t.Context()
		s, _, f := deciderService(t)
		delete(f.orgs, "o1")
		a, err := s.AccessOf(ctx, "u1")
		require.NoError(t, err)
		assert.Equal(t, a.Orgs, map[string]accounts.Role{"o2": accounts.Read})
		assert.Len(t, a.Repos, 0)
	})
	t.Run("inactive in the decision service", func(t *testing.T) {
		ctx := t.Context()
		s, _, f := deciderService(t)
		f.orgs["o1"] = accounts.OrgDecision{}
		_, err := s.RoleIn(ctx, p, "o1")
		assert.True(t, errors.Is(err, accounts.ErrForbidden), "%v", err)
	})
	t.Run("an error", func(t *testing.T) {
		ctx := t.Context()
		s, _, f := deciderService(t)
		f.err = errors.New("unavailable")
		_, err := s.AccessOf(ctx, "u1")
		assert.Error(t, err)
		assert.Error(t, s.Require(ctx, p, "o1", accounts.OrgMembership))
	})
	t.Run("a suspended account is refused before the decision service", func(t *testing.T) {
		ctx := t.Context()
		s, st, f := deciderService(t)
		at := time.Now()
		require.NoError(t, st.SetUserDisabled(ctx, "u1", &at))
		_, err := s.AccessOf(ctx, "u1")
		assert.True(t, errors.Is(err, accounts.ErrSuspended), "%v", err)
		assert.Len(t, f.asked, 0)
	})
	t.Run("a key of another organisation is refused before the decision service", func(t *testing.T) {
		ctx := t.Context()
		s, _, f := deciderService(t)
		_, err := s.RoleIn(ctx, accounts.Principal{User: accounts.User{ID: "u1"}, Via: "key", OrgID: "o2"}, "o1")
		assert.True(t, errors.Is(err, accounts.ErrForbidden), "%v", err)
		assert.Len(t, f.asked, 0)
	})
	t.Run("not a member in PostgreSQL", func(t *testing.T) {
		ctx := t.Context()
		s, st, f := deciderService(t)
		now := time.Now()
		require.NoError(t, st.CreateUser(ctx, accounts.User{ID: "u2", Name: "u2", Email: "u2@example.com", CreatedAt: now}))
		f.orgs["o1"] = accounts.OrgDecision{Active: true, Role: accounts.Owner, Perms: accounts.Owner.Bundle(accounts.ResourceOrganisation)}
		a, err := s.AccessOf(ctx, "u2")
		require.NoError(t, err)
		assert.Len(t, a.Orgs, 0, "OpenFGA never adds a membership")
		assert.Len(t, f.asked, 0)
	})
}
