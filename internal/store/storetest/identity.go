package storetest

import (
	"context"
	"testing"

	"github.com/astrona-io/codesynapse/internal/curation"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func identityContract(t *testing.T, st store.Store) {
	ctx := context.Background()
	repo := "identity-contract"
	t.Cleanup(func() { assert.NoError(t, st.DeleteRepo(ctx, repo)); assert.NoError(t, st.DeleteAllCurated(ctx, repo)) })
	g := model.NewGraph()
	rn := model.Node{ID: model.NodeID(repo, model.KindRepository, repo), Repo: repo, Kind: model.KindRepository, Identities: `{"version":1,"repo":"identity-contract","symbols":[]}`}
	n := model.Node{ID: model.NodeID(repo, model.KindFunction, "A"), Repo: repo, Kind: model.KindFunction, SymbolID: "sym-0123456789abcdef0123456789abcdef", Revision: "abc"}
	g.AddNode(rn)
	g.AddNode(n)
	require.NoError(t, st.ReplaceRepo(ctx, repo, g))
	got, err := st.Node(ctx, n.ID)
	require.NoError(t, err)
	assert.Equal(t, got.SymbolID, n.SymbolID)
	assert.Equal(t, got.Revision, n.Revision)
	got, err = st.Node(ctx, rn.ID)
	require.NoError(t, err)
	assert.Equal(t, got.Identities, rn.Identities)
	it := curation.Item{ID: "code1", Repo: repo, Kind: curation.KindCode, OrgID: "o1", Code: &curation.CodeRecord{CodeInput: curation.CodeInput{Method: "annotation", Destinations: []string{n.SymbolID}, Purpose: "entry point", Labels: []string{"api"}}, Sources: []curation.SymbolSource{}, Targets: []curation.SymbolSource{{ID: n.SymbolID, Revision: "abc", NodeID: n.ID, Fingerprint: "hash"}}}}
	it.Code.Method = "copy"
	it.Code.OriginRepo = "source-contract"
	it.Code.Origins = []string{"sym-abcdef0123456789abcdef0123456789"}
	it.Code.Sources = []curation.SymbolSource{{ID: it.Code.Origins[0], Repo: it.Code.OriginRepo, Revision: "origin-rev", NodeID: "source-contract|Function|Original", Fingerprint: "original-hash"}}
	it.Code.Targets[0].Repo = repo
	require.NoError(t, st.SaveCurated(ctx, it))
	saved, err := st.GetCurated(ctx, repo, it.ID)
	require.NoError(t, err)
	assert.Equal(t, saved, it)
	review := curation.Item{ID: "identity-review", Repo: repo, Kind: curation.KindIdentityReview, OrgID: "o1", CreatedBy: "u1", Client: "web", IdentityReview: &curation.IdentityReview{IdentityReviewInput: curation.IdentityReviewInput{From: it.Code.Origins[0], To: n.SymbolID, Relation: "same-symbol", Reason: "Reviewed history", Revision: "abc"}, Source: curation.SymbolSource{ID: it.Code.Origins[0], Repo: repo, Revision: "previous"}, Target: it.Code.Targets[0], Supersedes: []string{}, SourceFile: "old.go", TargetFile: "new.go"}}
	require.NoError(t, st.SaveCurated(ctx, review))
	stored, err := st.GetCurated(ctx, repo, review.ID)
	require.NoError(t, err)
	assert.Equal(t, stored, review)
}
