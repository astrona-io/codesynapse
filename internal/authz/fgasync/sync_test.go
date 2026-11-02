package fgasync

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/authz/fga"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// fakeClient holds tuples in memory and behaves like OpenFGA where the sync
// depends on it: writing a tuple it holds or deleting one it lacks fails
// the whole call, and reads are bounded.
type fakeClient struct {
	mu     sync.Mutex
	tuples map[fga.Tuple]bool
	calls  []string // "write:n" or "delete:n", in order
	writes [][]fga.Tuple
	failAt int // fail the call with this 1-based number; 0 never
	n      int
}

func newFake(tuples ...fga.Tuple) *fakeClient {
	f := &fakeClient{tuples: map[fga.Tuple]bool{}}
	for _, t := range tuples {
		f.tuples[t] = true
	}
	return f
}

func (f *fakeClient) StoreID() string { return "store" }
func (f *fakeClient) ModelID() string { return "model" }

func (f *fakeClient) Read(_ context.Context, filter fga.Tuple, maxTuples int) ([]fga.Tuple, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	typeOnly := strings.HasSuffix(filter.Object, ":")
	if typeOnly && (filter.User == "" || filter.Relation == "") {
		return nil, errors.New("fake: a type-only read needs a user and relation")
	}
	var out []fga.Tuple
	for t := range f.tuples {
		if typeOnly && !strings.HasPrefix(t.Object, filter.Object) || !typeOnly && t.Object != filter.Object {
			continue
		}
		if filter.User != "" && t.User != filter.User || filter.Relation != "" && t.Relation != filter.Relation {
			continue
		}
		out = append(out, t)
	}
	if len(out) > maxTuples {
		return nil, fga.ErrTruncated
	}
	return out, nil
}

func (f *fakeClient) change(tuples []fga.Tuple, write bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	if f.n == f.failAt {
		return errors.New("fake: unavailable")
	}
	if len(tuples) > fga.MaxWrite {
		return fga.ErrTooMany
	}
	for _, t := range tuples {
		if f.tuples[t] == write {
			return fmt.Errorf("fake: tuple %v exists=%v", t, f.tuples[t])
		}
	}
	for _, t := range tuples {
		if write {
			f.tuples[t] = true
		} else {
			delete(f.tuples, t)
		}
	}
	op := "delete"
	if write {
		op = "write"
		f.writes = append(f.writes, slices.Clone(tuples))
	}
	f.calls = append(f.calls, fmt.Sprintf("%s:%d", op, len(tuples)))
	return nil
}

func (f *fakeClient) WriteTuples(_ context.Context, ts []fga.Tuple) error { return f.change(ts, true) }
func (f *fakeClient) DeleteTuples(_ context.Context, ts []fga.Tuple) error {
	return f.change(ts, false)
}

func (f *fakeClient) held() []fga.Tuple {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fga.Tuple, 0, len(f.tuples))
	for t := range f.tuples {
		out = append(out, t)
	}
	sortTuples(out)
	return out
}

func tup(user, rel, obj string) fga.Tuple { return fga.Tuple{User: user, Relation: rel, Object: obj} }

func repoObj(org, graph string) string { return "repository:" + fga.RepositoryID(org, graph) }

func TestDesired(t *testing.T) {
	org := accounts.Org{ID: "o1", DefaultRepoAccess: accounts.DefaultRepoReadOnly, PolicyRevision: 3}
	tests := []struct {
		name    string
		policy  accounts.OrgPolicy
		want    []fga.Tuple
		wantErr bool
	}{
		{
			name: "members, default and guests",
			policy: accounts.OrgPolicy{Org: org, Members: []accounts.PolicyMember{
				{UserID: "u1", Role: accounts.Owner}, {UserID: "u2", Role: accounts.Read}, {UserID: "u3", Role: accounts.Guest},
			}},
			want: []fga.Tuple{
				tup("organisation:o1#member", "default_readers", "organisation:o1"),
				tup("user:u1", "member", "organisation:o1"), tup("user:u1", "owner", "organisation:o1"),
				tup("user:u2", "member", "organisation:o1"), tup("user:u2", "read", "organisation:o1"),
				tup("user:u3", "guest", "organisation:o1"),
			},
		},
		{
			name: "no default without READ_ONLY",
			policy: accounts.OrgPolicy{Org: accounts.Org{ID: "o1", DefaultRepoAccess: accounts.DefaultRepoNone},
				Members: []accounts.PolicyMember{{UserID: "u1", Role: accounts.Write}}},
			want: []fga.Tuple{tup("user:u1", "member", "organisation:o1"), tup("user:u1", "write", "organisation:o1")},
		},
		{
			name: "teams: anchor, role, read and full members",
			policy: accounts.OrgPolicy{Org: accounts.Org{ID: "o1"}, Teams: []accounts.PolicyTeam{
				{ID: "t1", Role: accounts.Full, Members: []accounts.PolicyTeamMember{{UserID: "u1"}, {UserID: "u2", Maintainer: true}}},
				{ID: "t2", Role: accounts.Guest},
			}},
			want: []fga.Tuple{
				tup("organisation:o1", "organisation", "team:t1"), tup("team:t1#read", "full", "organisation:o1"),
				tup("user:u1", "read", "team:t1"), tup("user:u2", "full", "team:t1"),
				tup("organisation:o1", "organisation", "team:t2"),
			},
		},
		{
			name: "repositories: org space, personal, grants and deny",
			policy: accounts.OrgPolicy{Org: accounts.Org{ID: "o1"}, Repos: []accounts.PolicyRepo{
				{Graph: "g/org", Users: []accounts.PolicyGrant{{SubjectID: "u1", Role: accounts.Deny}, {SubjectID: "u2", Role: accounts.Full}},
					Teams: []accounts.PolicyGrant{{SubjectID: "t1", Role: accounts.Write}}},
				{Graph: "g/mine", Personal: true, Users: []accounts.PolicyGrant{{SubjectID: "u3", Role: accounts.Read}}},
			}},
			want: []fga.Tuple{
				tup("organisation:o1", "organisation", repoObj("o1", "g/org")), tup("organisation:o1", "org_space", repoObj("o1", "g/org")),
				tup("user:u1", "direct_deny", repoObj("o1", "g/org")), tup("user:u2", "direct_full", repoObj("o1", "g/org")),
				tup("team:t1#read", "team_write", repoObj("o1", "g/org")),
				tup("organisation:o1", "organisation", repoObj("o1", "g/mine")),
				tup("user:u3", "direct_read", repoObj("o1", "g/mine")),
			},
		},
		{
			name: "custom roles: permissions, an empty direct grant still marks it",
			policy: accounts.OrgPolicy{Org: accounts.Org{ID: "o1"},
				Members: []accounts.PolicyMember{{UserID: "u1", Role: accounts.Guest, Perms: accounts.PermsOf(accounts.OrgAuditRead)}},
				Teams:   []accounts.PolicyTeam{{ID: "t1", Perms: accounts.PermsOf(accounts.OrgTeamsRead)}},
				Repos: []accounts.PolicyRepo{{Graph: "g", Personal: true,
					Users: []accounts.PolicyGrant{{SubjectID: "u1", Custom: true, Perms: accounts.PermsOf(accounts.RepoCodeRead, accounts.RepoReviewsWrite)}, {SubjectID: "u2", Custom: true}},
					Teams: []accounts.PolicyGrant{{SubjectID: "t1", Custom: true, Perms: accounts.PermsOf(accounts.RepoCodeRead)}}}},
			},
			want: []fga.Tuple{
				tup("user:u1", "guest", "organisation:o1"), tup("user:u1", "audit_read", "organisation:o1"),
				tup("organisation:o1", "organisation", "team:t1"), tup("team:t1#read", "teams_read", "organisation:o1"),
				tup("organisation:o1", "organisation", repoObj("o1", "g")),
				tup("user:u1", "direct_custom", repoObj("o1", "g")), tup("user:u1", "user_code_read", repoObj("o1", "g")),
				tup("user:u1", "user_reviews_read", repoObj("o1", "g")), tup("user:u1", "user_reviews_write", repoObj("o1", "g")),
				tup("user:u2", "direct_custom", repoObj("o1", "g")),
				tup("team:t1#read", "team_custom", repoObj("o1", "g")), tup("team:t1#read", "team_code_read", repoObj("o1", "g")),
			},
		},
		{name: "repository permission in an organisation role", policy: accounts.OrgPolicy{Org: accounts.Org{ID: "o1"}, Members: []accounts.PolicyMember{{UserID: "u1", Role: accounts.Read, Perms: accounts.PermsOf(accounts.RepoCodeRead)}}}, wantErr: true},
		{name: "marker in a role", policy: accounts.OrgPolicy{Org: accounts.Org{ID: "o1"}, Teams: []accounts.PolicyTeam{{ID: "t1", Perms: accounts.PermsOf(accounts.OrgOwnerRules)}}}, wantErr: true},
		{name: "bad organisation ID", policy: accounts.OrgPolicy{Org: accounts.Org{ID: "o#1"}}, wantErr: true},
		{name: "bad user ID", policy: accounts.OrgPolicy{Org: accounts.Org{ID: "o1"}, Members: []accounts.PolicyMember{{UserID: "user:x", Role: accounts.Read}}}, wantErr: true},
		{name: "bad membership role", policy: accounts.OrgPolicy{Org: accounts.Org{ID: "o1"}, Members: []accounts.PolicyMember{{UserID: "u1", Role: accounts.Deny}}}, wantErr: true},
		{name: "team deny", policy: accounts.OrgPolicy{Org: accounts.Org{ID: "o1"}, Repos: []accounts.PolicyRepo{{Graph: "g", Teams: []accounts.PolicyGrant{{SubjectID: "t1", Role: accounts.Deny}}}}}, wantErr: true},
		{name: "owner grant on a repository", policy: accounts.OrgPolicy{Org: accounts.Org{ID: "o1"}, Repos: []accounts.PolicyRepo{{Graph: "g", Users: []accounts.PolicyGrant{{SubjectID: "u1", Role: accounts.Owner}}}}}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Desired(tc.policy)
			if tc.wantErr {
				assert.True(t, errors.Is(err, accounts.ErrInvalidInput), "got %v", err)
				return
			}
			require.NoError(t, err)
			sortTuples(got)
			sortTuples(tc.want)
			assert.Equal(t, got, tc.want)
		})
	}
}

// harness is a memstore with two organisations, used for sync records.
func harness(t *testing.T) accounts.Store {
	t.Helper()
	ctx := context.Background()
	st := memstore.New()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	require.NoError(t, st.CreateUser(ctx, accounts.User{ID: "u1", Name: "u1", Email: "u1@example.com", CreatedAt: now}))
	require.NoError(t, st.CreateOrg(ctx, accounts.Org{ID: "o1", Name: "o1", Slug: "o1", CreatedAt: now}, "u1"))
	require.NoError(t, st.CreateOrg(ctx, accounts.Org{ID: "o2", Name: "o2", Slug: "o2", CreatedAt: now}, "u1"))
	return st
}

func records(t *testing.T, st accounts.Store) accounts.OpenFGASyncStore {
	t.Helper()
	fs, ok := st.(accounts.OpenFGASyncStore)
	require.True(t, ok)
	return fs
}

// staticPolicy answers OrgPolicy from a map, with the store's revision.
func staticPolicy(st accounts.Store, policies map[string]accounts.OrgPolicy) func(context.Context, string) (accounts.OrgPolicy, error) {
	return func(ctx context.Context, orgID string) (accounts.OrgPolicy, error) {
		p, ok := policies[orgID]
		if !ok {
			return p, accounts.ErrNotFound
		}
		org, err := st.OrgByID(ctx, orgID)
		if err != nil {
			return p, err
		}
		p.Org.ID, p.Org.PolicyRevision = org.ID, org.PolicyRevision
		return p, nil
	}
}

func TestSyncRepairsDriftAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := harness(t)
	policy := accounts.OrgPolicy{
		Members: []accounts.PolicyMember{{UserID: "u1", Role: accounts.Owner}},
		Teams:   []accounts.PolicyTeam{{ID: "t1", Members: []accounts.PolicyTeamMember{{UserID: "u1"}}}},
		Repos:   []accounts.PolicyRepo{{Graph: "g/a", Teams: []accounts.PolicyGrant{{SubjectID: "t1", Role: accounts.Read}}}},
	}
	other := tup("user:u1", "direct_full", repoObj("o2", "g/b"))
	fake := newFake(
		// Drift: a stale grant on a desired repository, a removed team
		// still anchored, and a stale organisation level.
		tup("user:u9", "direct_full", repoObj("o1", "g/a")),
		tup("organisation:o1", "organisation", "team:old"), tup("user:u9", "read", "team:old"),
		tup("user:u9", "write", "organisation:o1"),
		// Another organisation's tuples are not this sync's business.
		tup("organisation:o2", "organisation", repoObj("o2", "g/b")), other,
	)
	s := &Syncer{Client: fake, Records: records(t, st), Policy: staticPolicy(st, map[string]accounts.OrgPolicy{"o1": policy})}

	res, err := s.Sync(ctx, "o1")
	require.NoError(t, err)
	assert.Equal(t, res.Deleted, 4)
	want, err := Desired(accounts.OrgPolicy{Org: accounts.Org{ID: "o1"}, Members: policy.Members, Teams: policy.Teams, Repos: policy.Repos})
	require.NoError(t, err)
	assert.Equal(t, res.Added, len(want))
	want = append(want, tup("organisation:o2", "organisation", repoObj("o2", "g/b")), other)
	sortTuples(want)
	assert.Equal(t, fake.held(), want, "OpenFGA holds exactly the policy, other organisations untouched")

	org, err := st.OrgByID(ctx, "o1")
	require.NoError(t, err)
	rec, err := records(t, st).OpenFGASynced(ctx, "o1")
	require.NoError(t, err)
	assert.Equal(t, rec.Revision, org.PolicyRevision)
	assert.Equal(t, rec.StoreID, "store")
	assert.Equal(t, rec.ModelID, "model")
	require.NoError(t, Fresh(ctx, records(t, st), "store", "model", org))

	calls := len(fake.calls)
	res, err = s.Sync(ctx, "o1")
	require.NoError(t, err)
	assert.False(t, res.Drift(), "a second sync finds nothing to do")
	assert.Len(t, fake.calls, calls, "and writes nothing")

	res, err = s.Verify(ctx, "o1")
	require.NoError(t, err)
	assert.False(t, res.Drift())
	fake.tuples[tup("user:u9", "direct_full", repoObj("o1", "g/a"))] = true
	res, err = s.Verify(ctx, "o1")
	require.NoError(t, err)
	assert.Equal(t, res.Deleted, 1, "verify reports drift")
	assert.Len(t, fake.calls, calls, "verify writes nothing")
}

func TestSyncOrderAndBatches(t *testing.T) {
	ctx := context.Background()
	st := harness(t)
	var grants []accounts.PolicyGrant
	for i := range 230 {
		grants = append(grants, accounts.PolicyGrant{SubjectID: fmt.Sprintf("u%03d", i), Role: accounts.Read})
	}
	policy := accounts.OrgPolicy{Repos: []accounts.PolicyRepo{{Graph: "g/new", Users: grants}}}
	fake := newFake(
		tup("organisation:o1", "organisation", repoObj("o1", "g/old")),
		tup("organisation:o1", "org_space", repoObj("o1", "g/old")),
	)
	s := &Syncer{Client: fake, Records: records(t, st), Policy: staticPolicy(st, map[string]accounts.OrgPolicy{"o1": policy})}
	_, err := s.Sync(ctx, "o1")
	require.NoError(t, err)
	// Delete org_space, then the anchor; write the anchor, then the rest
	// (org_space and 230 grants) in batches OpenFGA accepts.
	assert.Equal(t, fake.calls, []string{"delete:1", "delete:1", "write:1", "write:100", "write:100", "write:31"})
	assert.Equal(t, fake.writes[0], []fga.Tuple{tup("organisation:o1", "organisation", repoObj("o1", "g/new"))}, "the anchor is written first")
}

func TestSyncFailureKeepsTheOldRevision(t *testing.T) {
	ctx := context.Background()
	st := harness(t)
	var grants []accounts.PolicyGrant
	for i := range 150 {
		grants = append(grants, accounts.PolicyGrant{SubjectID: fmt.Sprintf("u%03d", i), Role: accounts.Write})
	}
	policy := accounts.OrgPolicy{Repos: []accounts.PolicyRepo{{Graph: "g", Users: grants}}}
	fake := newFake()
	fake.failAt = 3 // after the anchor and the first batch of 100
	fs := records(t, st)
	s := &Syncer{Client: fake, Records: fs, Policy: staticPolicy(st, map[string]accounts.OrgPolicy{"o1": policy})}
	_, err := s.Sync(ctx, "o1")
	require.Error(t, err)
	_, err = fs.OpenFGASynced(ctx, "o1")
	assert.True(t, errors.Is(err, accounts.ErrNotFound), "nothing recorded after a failed write")
	org, err := st.OrgByID(ctx, "o1")
	require.NoError(t, err)
	assert.True(t, errors.Is(Fresh(ctx, fs, "store", "model", org), ErrStale))

	// The next sync finishes from what OpenFGA then holds.
	res, err := s.Sync(ctx, "o1")
	require.NoError(t, err)
	assert.Equal(t, res.Added, 51)
	require.NoError(t, Fresh(ctx, fs, "store", "model", org))
}

func TestSyncPendingSkipsAFailingOrganisation(t *testing.T) {
	ctx := context.Background()
	st := harness(t)
	fs := records(t, st)
	policies := map[string]accounts.OrgPolicy{
		"o1": {Members: []accounts.PolicyMember{{UserID: "bad:id", Role: accounts.Read}}},
		"o2": {Members: []accounts.PolicyMember{{UserID: "u1", Role: accounts.Owner}}},
	}
	s := &Syncer{Client: newFake(), Records: fs, Policy: staticPolicy(st, policies)}
	done, err := s.SyncPending(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, done, 1)
	_, err = fs.OpenFGASynced(ctx, "o1")
	assert.True(t, errors.Is(err, accounts.ErrNotFound), "invalid policy is never recorded as synced")
	pending, err := fs.OpenFGAPending(ctx, "store", "model", 10)
	require.NoError(t, err)
	assert.Len(t, pending, 1)

	// A rebuild request makes a fresh organisation pending again without
	// making it stale.
	_, err = fs.RequestOpenFGARebuild(ctx, "o2", time.Now())
	require.NoError(t, err)
	pending, err = fs.OpenFGAPending(ctx, "store", "model", 10)
	require.NoError(t, err)
	assert.Len(t, pending, 2)
	o2, err := st.OrgByID(ctx, "o2")
	require.NoError(t, err)
	require.NoError(t, Fresh(ctx, fs, "store", "model", o2))
}

func TestFresh(t *testing.T) {
	ctx := context.Background()
	st := harness(t)
	fs := records(t, st)
	org, err := st.OrgByID(ctx, "o1")
	require.NoError(t, err)
	assert.True(t, errors.Is(Fresh(ctx, fs, "store", "model", org), ErrStale), "never synced")
	require.NoError(t, fs.RecordOpenFGASync(ctx, accounts.OpenFGASync{OrgID: "o1", StoreID: "store", ModelID: "model", Revision: org.PolicyRevision, SyncedAt: time.Now()}, time.Now()))
	tests := []struct {
		name           string
		store, model   string
		revisionOffset int64
		stale          bool
	}{
		{name: "current", store: "store", model: "model"},
		{name: "newer policy", store: "store", model: "model", revisionOffset: 1, stale: true},
		{name: "another store", store: "other", model: "model", stale: true},
		{name: "another model", store: "store", model: "other", stale: true},
		{name: "no store", model: "model", stale: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := org
			o.PolicyRevision += tc.revisionOffset
			err := Fresh(ctx, fs, tc.store, tc.model, o)
			assert.Equal(t, errors.Is(err, ErrStale), tc.stale, "got %v", err)
		})
	}
}
