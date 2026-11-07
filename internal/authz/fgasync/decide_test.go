package fgasync

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/authz/fga"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// fakeChecker answers checks from a set of allowed tuples.
type fakeChecker struct {
	mu        sync.Mutex
	allowed   map[fga.Tuple]bool
	err       error
	truncate  map[string]bool // relations whose list is cut
	batches   int
	lists     int
	store     string
	modelName string
}

func (f *fakeChecker) StoreID() string { return f.store }
func (f *fakeChecker) ModelID() string { return f.modelName }

func (f *fakeChecker) BatchCheck(_ context.Context, checks []fga.Tuple) ([]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches++
	if len(checks) > fga.MaxBatchCheck {
		return nil, fga.ErrTooMany
	}
	if f.err != nil {
		return nil, f.err
	}
	out := make([]bool, len(checks))
	for i, c := range checks {
		out[i] = f.allowed[c]
	}
	return out, nil
}

func (f *fakeChecker) ListObjects(_ context.Context, user, relation, objectType string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	if f.err != nil {
		return nil, f.err
	}
	if f.truncate[relation] {
		return nil, fga.ErrTruncated
	}
	var out []string
	for t := range f.allowed {
		if t.User == user && t.Relation == relation && len(t.Object) > len(objectType) && t.Object[:len(objectType)+1] == objectType+":" {
			out = append(out, t.Object)
		}
	}
	return out, nil
}

// allow grants user the relations on obj.
func (f *fakeChecker) allow(user, obj string, rels ...string) {
	for _, r := range rels {
		f.allowed[fga.Tuple{User: user, Relation: r, Object: obj}] = true
	}
}

// repoRels are the relations of a repository role's bundle.
func repoRels(r accounts.Role) []string {
	var out []string
	for _, p := range r.Permissions(accounts.ResourceRepository) {
		out = append(out, Relation(p))
	}
	return out
}

func orgRels(r accounts.Role) []string {
	out := []string{"active"}
	for _, l := range []accounts.Role{accounts.Read, accounts.Write, accounts.Full, accounts.Owner} {
		if r.AtLeast(l) {
			out = append(out, orgLevel[l])
		}
	}
	for _, p := range r.Permissions(accounts.ResourceOrganisation) {
		out = append(out, Relation(p))
	}
	return out
}

func decider(t *testing.T, ck *fakeChecker) (*Decider, accounts.Store) {
	t.Helper()
	st := harness(t)
	ctx := t.Context()
	for _, id := range []string{"o1", "o2"} {
		org, err := st.OrgByID(ctx, id)
		require.NoError(t, err)
		require.NoError(t, records(t, st).RecordOpenFGASync(ctx, accounts.OpenFGASync{OrgID: id, StoreID: "store", ModelID: "model", Revision: org.PolicyRevision, SyncedAt: time.Now()}, time.Now()))
	}
	return &Decider{Records: records(t, st), Select: func(context.Context) (Checker, error) { return ck, nil }}, st
}

func newChecker() *fakeChecker {
	return &fakeChecker{allowed: map[fga.Tuple]bool{}, truncate: map[string]bool{}, store: "store", modelName: "model"}
}

func orgsOf(t *testing.T, st accounts.Store, ids ...string) []accounts.Org {
	t.Helper()
	var out []accounts.Org
	for _, id := range ids {
		o, err := st.OrgByID(t.Context(), id)
		require.NoError(t, err)
		out = append(out, o)
	}
	return out
}

func TestDecideAnswersFromOpenFGA(t *testing.T) {
	for _, many := range []bool{false, true} {
		name := "batch check"
		if many {
			name = "list objects"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			ck := newChecker()
			d, st := decider(t, ck)
			ck.allow("user:u1", "organisation:o1", orgRels(accounts.Write)...)
			ck.allow("user:u1", repoObj("o1", "g/a"), repoRels(accounts.Full)...)
			ck.allow("user:u1", repoObj("o1", "g/b"), repoRels(accounts.Read)...)
			// Granted in OpenFGA but not asked about: never in the answer.
			ck.allow("user:u1", repoObj("o1", "g/other"), repoRels(accounts.Full)...)
			q := accounts.DecisionRequest{UserID: "u1", Orgs: orgsOf(t, st, "o1"), Repos: []accounts.RepoRef{{OrgID: "o1", Graph: "g/a"}, {OrgID: "o1", Graph: "g/b"}, {OrgID: "o1", Graph: "g/none"}}}
			if many {
				for i := range maxBatchChecks + 50 {
					q.Repos = append(q.Repos, accounts.RepoRef{OrgID: "o1", Graph: "g/x" + string(rune('a'+i%26)) + string(rune('a'+i/26))})
				}
			}
			got, err := d.Decide(ctx, q)
			require.NoError(t, err)
			assert.Equal(t, got.Orgs["o1"], accounts.OrgDecision{Active: true, Role: accounts.Write, Perms: accounts.Write.Bundle(accounts.ResourceOrganisation)})
			assert.Equal(t, got.Repos, map[string]accounts.Perms{
				accounts.RepoObject("o1", "g/a"): accounts.Full.Bundle(accounts.ResourceRepository),
				accounts.RepoObject("o1", "g/b"): accounts.Read.Bundle(accounts.ResourceRepository),
			})
			assert.Equal(t, ck.lists > 0, many, "many repositories are listed per relation")
		})
	}
}

func TestDecideGuestAndInactive(t *testing.T) {
	ctx := t.Context()
	ck := newChecker()
	d, st := decider(t, ck)
	ck.allow("user:u1", "organisation:o1", "active", Relation(accounts.OrgMembership))
	// o2: levels without active (a suspended or removed member) give nothing.
	ck.allow("user:u1", "organisation:o2", "read", "write")
	ck.allow("user:u1", repoObj("o2", "g/a"), repoRels(accounts.Full)...)
	got, err := d.Decide(ctx, accounts.DecisionRequest{UserID: "u1", Orgs: orgsOf(t, st, "o1", "o2"), Repos: []accounts.RepoRef{{OrgID: "o2", Graph: "g/a"}}})
	require.NoError(t, err)
	assert.Equal(t, got.Orgs["o1"].Role, accounts.Guest)
	assert.True(t, got.Orgs["o1"].Perms.Has(accounts.OrgMembership))
	assert.False(t, got.Orgs["o2"].Active)
	assert.Len(t, got.Repos, 0, "no repository of an organisation the user is not active in")
}

func TestDecideRefuses(t *testing.T) {
	t.Run("stale organisation is left out", func(t *testing.T) {
		ctx := t.Context()
		ck := newChecker()
		d, st := decider(t, ck)
		ck.allow("user:u1", "organisation:o1", orgRels(accounts.Owner)...)
		ck.allow("user:u1", "organisation:o2", orgRels(accounts.Owner)...)
		ck.allow("user:u1", repoObj("o2", "g/a"), repoRels(accounts.Full)...)
		orgs := orgsOf(t, st, "o1", "o2")
		orgs[1].PolicyRevision++ // a policy change OpenFGA has not seen
		got, err := d.Decide(ctx, accounts.DecisionRequest{UserID: "u1", Orgs: orgs, Repos: []accounts.RepoRef{{OrgID: "o2", Graph: "g/a"}}})
		require.NoError(t, err)
		_, ok := got.Orgs["o2"]
		assert.False(t, ok, "stale")
		assert.True(t, got.Orgs["o1"].Active)
		assert.Len(t, got.Repos, 0)
	})
	t.Run("other model is stale", func(t *testing.T) {
		ctx := t.Context()
		ck := newChecker()
		d, st := decider(t, ck)
		ck.modelName = "newer"
		ck.allow("user:u1", "organisation:o1", orgRels(accounts.Owner)...)
		got, err := d.Decide(ctx, accounts.DecisionRequest{UserID: "u1", Orgs: orgsOf(t, st, "o1")})
		require.NoError(t, err)
		assert.Len(t, got.Orgs, 0)
	})
	t.Run("error", func(t *testing.T) {
		ctx := t.Context()
		ck := newChecker()
		d, st := decider(t, ck)
		ck.err = errors.New("unavailable")
		_, err := d.Decide(ctx, accounts.DecisionRequest{UserID: "u1", Orgs: orgsOf(t, st, "o1")})
		require.Error(t, err)
	})
	t.Run("no store yet", func(t *testing.T) {
		ctx := t.Context()
		st := harness(t)
		d := &Decider{Records: records(t, st), Select: func(context.Context) (Checker, error) { return nil, fga.ErrNoStore }}
		_, err := d.Decide(ctx, accounts.DecisionRequest{UserID: "u1", Orgs: orgsOf(t, st, "o1")})
		assert.ErrorIs(t, err, fga.ErrNoStore)
	})
	t.Run("truncated list is checked one by one", func(t *testing.T) {
		ctx := t.Context()
		ck := newChecker()
		d, st := decider(t, ck)
		ck.allow("user:u1", "organisation:o1", orgRels(accounts.Read)...)
		q := accounts.DecisionRequest{UserID: "u1", Orgs: orgsOf(t, st, "o1")}
		for i := range 500 {
			g := "g/" + string(rune('a'+i%26)) + string(rune('a'+i/26%26))
			q.Repos = append(q.Repos, accounts.RepoRef{OrgID: "o1", Graph: g})
		}
		ck.allow("user:u1", repoObj("o1", "g/aa"), repoRels(accounts.Read)...)
		ck.truncate[Relation(accounts.RepoCodeRead)] = true
		got, err := d.Decide(ctx, q)
		require.NoError(t, err)
		assert.Equal(t, got.Repos, map[string]accounts.Perms{accounts.RepoObject("o1", "g/aa"): accounts.Read.Bundle(accounts.ResourceRepository)})
	})
	t.Run("truncated list too long to check refuses", func(t *testing.T) {
		ctx := t.Context()
		ck := newChecker()
		d, st := decider(t, ck)
		ck.allow("user:u1", "organisation:o1", orgRels(accounts.Read)...)
		q := accounts.DecisionRequest{UserID: "u1", Orgs: orgsOf(t, st, "o1")}
		for i := range maxFallbackRepos + 1 {
			q.Repos = append(q.Repos, accounts.RepoRef{OrgID: "o1", Graph: "g/" + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676))})
		}
		ck.truncate[Relation(accounts.RepoCodeRead)] = true
		_, err := d.Decide(ctx, q)
		assert.ErrorIs(t, err, fga.ErrTruncated)
	})
}
