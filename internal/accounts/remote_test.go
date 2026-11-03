package accounts

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

type fakeRemote struct {
	answer Access
	err    error
	asked  []AccessQuery
}

func (f *fakeRemote) AccessIn(_ context.Context, _ Principal, q AccessQuery) (Access, error) {
	f.asked = append(f.asked, q)
	return f.answer, f.err
}

func TestAccessQueryValidate(t *testing.T) {
	many := make([]string, MaxAccessGraphs+1)
	for i := range many {
		many[i] = "g"
	}
	for name, tc := range map[string]struct {
		q  AccessQuery
		ok bool
	}{
		"one organisation":        {AccessQuery{OrgID: "o"}, true},
		"some graphs":             {AccessQuery{OrgID: "o", Graphs: []string{"a", "b"}}, true},
		"organisation only":       {AccessQuery{OrgID: "o", OrgOnly: true}, true},
		"no organisation":         {AccessQuery{}, false},
		"too many graphs":         {AccessQuery{OrgID: "o", Graphs: many}, false},
		"org only with graphs":    {AccessQuery{OrgID: "o", OrgOnly: true, Graphs: []string{"a"}}, false},
		"an empty graph":          {AccessQuery{OrgID: "o", Graphs: []string{""}}, false},
		"an overlong graph name":  {AccessQuery{OrgID: "o", Graphs: []string{strings.Repeat("g", 513)}}, false},
		"the most graphs allowed": {AccessQuery{OrgID: "o", Graphs: many[:MaxAccessGraphs]}, true},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.q.Validate()
			assert.Equal(t, err == nil, tc.ok, "error: %v", err)
			if err != nil {
				assert.True(t, errors.Is(err, ErrInvalidInput))
			}
		})
	}
}

func TestAccessIn(t *testing.T) {
	a := Access{UserID: "u",
		Orgs:     map[string]Role{"o": Write, "x": Owner},
		OrgPerms: map[string]Perms{"o": AllPerms(), "x": AllPerms()},
		Teams:    map[string]string{"t": "read"},
		Repos:    []RepoAccess{{OrgID: "o", Graph: "a", Perms: AllPerms()}, {OrgID: "o", Graph: "b", Perms: AllPerms()}, {OrgID: "x", Graph: "a", Perms: AllPerms()}},
	}
	got := a.In(AccessQuery{OrgID: "o"})
	assert.Equal(t, len(got.Orgs), 1)
	assert.Equal(t, len(got.OrgPerms), 1)
	assert.Equal(t, len(got.Teams), 0, "team IDs never travel")
	assert.Equal(t, len(got.Repos), 2, "only the organisation asked about")
	got = a.In(AccessQuery{OrgID: "o", Graphs: []string{"b"}})
	require.Equal(t, len(got.Repos), 1)
	assert.Equal(t, got.Repos[0].Graph, "b")
	got = a.In(AccessQuery{OrgID: "o", OrgOnly: true})
	assert.Equal(t, len(got.Repos), 0)
	assert.Equal(t, got.Orgs["o"], Write)

	ctx := t.Context()
	p := Principal{User: User{ID: "u"}, Via: "session"}
	t.Run("the remote answers and is filtered again", func(t *testing.T) {
		f := &fakeRemote{answer: a}
		s := &Service{Remote: f}
		got, err := s.AccessIn(ctx, p, AccessQuery{OrgID: "o", Graphs: []string{"a"}})
		require.NoError(t, err)
		assert.Equal(t, len(got.Repos), 1)
		assert.Equal(t, len(got.Orgs), 1)
		assert.Equal(t, len(f.asked), 1)
	})
	t.Run("an answer about someone else is refused", func(t *testing.T) {
		other := a
		other.UserID = "v"
		s := &Service{Remote: &fakeRemote{answer: other}}
		_, err := s.AccessIn(ctx, p, AccessQuery{OrgID: "o"})
		assert.True(t, errors.Is(err, ErrForbidden), "%v", err)
	})
	t.Run("a remote error refuses", func(t *testing.T) {
		s := &Service{Remote: &fakeRemote{err: errors.New("unreachable")}}
		_, err := s.AccessIn(ctx, p, AccessQuery{OrgID: "o"})
		assert.True(t, err != nil)
	})
	t.Run("a key bound to another organisation is never asked about this one", func(t *testing.T) {
		f := &fakeRemote{answer: a}
		s := &Service{Remote: f}
		_, err := s.AccessIn(ctx, Principal{User: User{ID: "u"}, Via: "key", OrgID: "x"}, AccessQuery{OrgID: "o"})
		assert.True(t, errors.Is(err, ErrNotFound))
		assert.Equal(t, len(f.asked), 0)
	})
	t.Run("an invalid query is not sent", func(t *testing.T) {
		f := &fakeRemote{answer: a}
		s := &Service{Remote: f}
		_, err := s.AccessIn(ctx, p, AccessQuery{})
		assert.True(t, errors.Is(err, ErrInvalidInput))
		assert.Equal(t, len(f.asked), 0)
	})
}
