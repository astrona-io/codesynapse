package storetest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/curation"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/store"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

// curatedContract checks CuratedStore: items round trip, list newest first
// and by kind, repositories do not see each other's, saving again replaces,
// and the graph's own lifecycle (ReplaceRepo, DeleteRepo) leaves them alone.
func curatedContract(t *testing.T, db store.Store) {
	ctx := context.Background()
	repo := fmt.Sprintf("curated-fixture-%d", time.Now().UnixNano())
	other := repo + "-other"
	t.Cleanup(func() {
		assert.NoError(t, db.DeleteAllCurated(ctx, repo))
		assert.NoError(t, db.DeleteAllCurated(ctx, other))
		assert.NoError(t, db.DeleteRepo(ctx, repo))
	})
	at := func(i int) string { return time.Date(2026, 10, 1, 0, i, 0, 0, time.UTC).Format(time.RFC3339) }
	proposal := func(id string, i int) curation.Item {
		return curation.Item{ID: id, Repo: repo, Kind: curation.KindProposal, CreatedAt: at(i), UpdatedAt: at(i), OrgID: "org1", CreatedBy: "u1", Client: "claude-code",
			Proposal: &curation.Proposal{Type: "adr", Title: "Use " + id, Status: "proposed", State: curation.Open, Components: []string{"api"}, SuggestedID: "ADR-0009"}}
	}
	link := func(id string, i int) curation.Item {
		return curation.Item{ID: id, Repo: repo, Kind: curation.KindLink, CreatedAt: at(i), UpdatedAt: at(i),
			Link: &curation.Link{Kind: curation.Implements, Record: "ADR-0001", Target: "internal/store"}}
	}

	got, err := db.ListCurated(ctx, repo, "", 10)
	require.NoError(t, err)
	assert.Len(t, got, 0)

	for _, it := range []curation.Item{proposal("p1", 1), link("l1", 2), proposal("p2", 3)} {
		require.NoError(t, db.SaveCurated(ctx, it))
	}
	require.NoError(t, db.SaveCurated(ctx, curation.Item{ID: "x1", Repo: other, Kind: curation.KindLink, CreatedAt: at(1), Link: &curation.Link{Kind: curation.Relates, Record: "A", Target: "B"}}))

	all, err := db.ListCurated(ctx, repo, "", 10)
	require.NoError(t, err)
	require.Len(t, all, 3)
	assert.Equal(t, all[0].ID, "p2", "newest first")
	assert.Equal(t, all[2].ID, "p1")
	props, err := db.ListCurated(ctx, repo, curation.KindProposal, 10)
	require.NoError(t, err)
	assert.Len(t, props, 2)
	links, err := db.ListCurated(ctx, repo, curation.KindLink, 10)
	require.NoError(t, err)
	require.Len(t, links, 1)
	assert.Equal(t, links[0].Link.Target, "internal/store")
	limited, err := db.ListCurated(ctx, repo, "", 1)
	require.NoError(t, err)
	assert.Len(t, limited, 1)

	one, err := db.GetCurated(ctx, repo, "p1")
	require.NoError(t, err)
	assert.Equal(t, one.Proposal.Title, "Use p1")
	assert.Equal(t, one.Proposal.Components, []string{"api"})
	assert.Equal(t, one.Client, "claude-code")
	assert.Equal(t, one.OrgID, "org1")
	_, err = db.GetCurated(ctx, other, "p1")
	assert.ErrorIs(t, err, store.ErrNotFound, "another repository's item is not reachable by ID")
	_, err = db.GetCurated(ctx, repo, "nope")
	assert.ErrorIs(t, err, store.ErrNotFound)

	changed := proposal("p1", 1)
	changed.Proposal.State = curation.Withdrawn
	require.NoError(t, db.SaveCurated(ctx, changed))
	one, err = db.GetCurated(ctx, repo, "p1")
	require.NoError(t, err)
	assert.Equal(t, one.Proposal.State, curation.Withdrawn, "saving again replaces")
	all, _ = db.ListCurated(ctx, repo, "", 10)
	assert.Len(t, all, 3)

	// The graph's lifecycle does not touch them.
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID(repo, model.KindRepository, repo), Repo: repo, Kind: model.KindRepository, Name: repo})
	require.NoError(t, db.ReplaceRepo(ctx, repo, g))
	require.NoError(t, db.DeleteRepo(ctx, repo))
	all, err = db.ListCurated(ctx, repo, "", 10)
	require.NoError(t, err)
	assert.Len(t, all, 3)

	require.NoError(t, db.DeleteCurated(ctx, repo, "l1"))
	all, _ = db.ListCurated(ctx, repo, "", 10)
	assert.Len(t, all, 2)
	require.NoError(t, db.DeleteAllCurated(ctx, repo))
	all, _ = db.ListCurated(ctx, repo, "", 10)
	assert.Len(t, all, 0)
	o, err := db.ListCurated(ctx, other, "", 10)
	require.NoError(t, err)
	assert.Len(t, o, 1, "another repository's items stay")
}
