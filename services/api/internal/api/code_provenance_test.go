package api

import (
	"context"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/accounts"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/store"
	"github.com/parisnakitakejser/codesynapse/internal/store/memory"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func TestReuseSourceReaderRequiresSameOrganisation(t *testing.T) {
	db := memory.New()
	g := model.NewGraph()
	n := model.Node{ID: model.NodeID("source", model.KindRepository, "source"), Repo: "source", Kind: model.KindRepository}
	g.AddNode(n)
	require.NoError(t, db.ReplaceRepo(context.Background(), "source", g))
	s := &Server{st: db}
	for _, tt := range []struct {
		name, sourceOrg             string
		visible, ambiguous, allowed bool
	}{
		{"same organisation", "o1", true, false, true},
		{"different organisation", "o2", true, false, false},
		{"source revoked", "o1", false, false, false},
		{"ambiguous source", "o1", true, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sc := scope{graphs: map[string]accounts.Role{"destination": accounts.Member}, reach: map[string]accounts.Reach{"destination": {OrgID: "o1", Role: accounts.Member}}}
			if tt.visible {
				sc.graphs["source"] = accounts.Viewer
				sc.reach["source"] = accounts.Reach{OrgID: tt.sourceOrg, Role: accounts.Viewer, Ambiguous: tt.ambiguous}
			}
			ctx := context.WithValue(context.Background(), scopeKey{}, sc)
			r, err := s.catalogOf(ctx, "destination").ReadRepository(ctx, "source")
			if !tt.allowed {
				assert.ErrorIs(t, err, store.ErrNotFound)
				return
			}
			require.NoError(t, err)
			got, err := r.Node(ctx, n.ID)
			require.NoError(t, err)
			assert.Equal(t, got.Repo, "source")
			_, err = r.Node(ctx, model.NodeID("destination", model.KindRepository, "destination"))
			assert.ErrorIs(t, err, store.ErrNotFound)
		})
	}
}
