package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/curation"
	"github.com/astrona-io/codesynapse/internal/identity"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func reviewFixture(t *testing.T) (Service, *memory.Store, curation.IdentityReviewInput, *model.Graph) {
	t.Helper()
	ctx := context.Background()
	db := memory.New()
	g, first := codeGraph(t, "r", identity.Report{Version: 1, Repo: "r"}, "one", "body")
	next := model.NewGraph()
	for _, n := range g.Nodes() {
		if n.Name == "A" {
			continue
		}
		if n.Kind == model.KindRepository {
			n.Head = "two"
		}
		next.AddNode(n)
	}
	report, err := identity.Reconcile(next, "r", first)
	require.NoError(t, err)
	rn, _ := next.Node(model.NodeID("r", model.KindRepository, "r"))
	rn.Identities, err = identity.Encode(report)
	require.NoError(t, err)
	require.NoError(t, db.ReplaceRepo(ctx, "r", next))
	in := curation.IdentityReviewInput{From: first.Symbols[0].ID, To: first.Symbols[1].ID, Relation: "same-symbol", Reason: "Reviewed implementation and history", Revision: "two"}
	return Service{R: store.ForRepository(db, "r"), Items: db, Org: agent.Org, Now: fixedNow}, db, in, next
}

func TestReviewedIdentityScopeAndSupersession(t *testing.T) {
	ctx := context.Background()
	svc, db, in, _ := reviewFixture(t)
	_, err := svc.ResolveSymbol(ctx, "r", in.From)
	assert.ErrorIs(t, err, store.ErrNotFound)
	review, err := svc.ReviewIdentity(ctx, "r", agent, in)
	require.NoError(t, err)
	assert.Equal(t, review.Item.IdentityReview.Source.Revision, "one")
	n, err := svc.ResolveSymbol(ctx, "r", in.From)
	require.NoError(t, err)
	assert.Equal(t, n.SymbolID, in.To)
	other := svc
	other.Org = "other"
	_, err = other.ResolveSymbol(ctx, "r", in.From)
	assert.ErrorIs(t, err, store.ErrNotFound)
	page, err := other.IdentityReviews(ctx, "r", "", 0, 20)
	require.NoError(t, err)
	assert.Equal(t, page.Total, 0)
	in.Relation = "copy"
	copyReview, err := svc.ReviewIdentity(ctx, "r", agent, in)
	require.NoError(t, err)
	assert.Equal(t, copyReview.Item.IdentityReview.Supersedes, []string{review.Item.ID})
	_, err = svc.ResolveSymbol(ctx, "r", in.From)
	assert.ErrorIs(t, err, store.ErrNotFound, "copies never become aliases")
	page, err = svc.IdentityReviews(ctx, "r", "", 0, 1)
	require.NoError(t, err)
	assert.Equal(t, page.Total, 2)
	assert.True(t, page.Truncated)
	conflict := copyReview.Item
	conflict.ID = curation.NewID("identity")
	r := *conflict.IdentityReview
	conflict.IdentityReview = &r
	r.Relation = "same-symbol"
	require.NoError(t, db.SaveCurated(ctx, conflict))
	_, err = svc.ResolveSymbol(ctx, "r", in.From)
	assert.ErrorIs(t, err, ErrIdentityConflict)
	in.Relation = "same-symbol"
	_, err = svc.ReviewIdentity(ctx, "r", agent, in)
	require.NoError(t, err)
	_, err = svc.ResolveSymbol(ctx, "r", in.From)
	require.NoError(t, err)
}

func TestIdentityReviewRejectsStaleLiveAndUnauditedChanges(t *testing.T) {
	ctx := context.Background()
	svc, db, in, _ := reviewFixture(t)
	stale := in
	stale.Revision = "old"
	_, err := svc.ReviewIdentity(ctx, "r", agent, stale)
	assert.ErrorIs(t, err, curation.ErrInvalid)
	live := in
	live.From = in.To
	live.To = in.From
	_, err = svc.ReviewIdentity(ctx, "r", agent, live)
	assert.ErrorIs(t, err, curation.ErrInvalid)
	failed := errors.New("audit failed")
	svc.BeforeIdentityReview = func(context.Context, string) error { return failed }
	_, err = svc.ReviewIdentity(ctx, "r", agent, in)
	assert.ErrorIs(t, err, failed)
	items, err := db.ListCurated(ctx, "r", curation.KindIdentityReview, 20)
	require.NoError(t, err)
	assert.Len(t, items, 0)
}

func TestIdentityReviewRespectsIgnoredEvidence(t *testing.T) {
	ctx := context.Background()
	svc, db, in, g := reviewFixture(t)
	_, err := svc.ReviewIdentity(ctx, "r", agent, in)
	require.NoError(t, err)
	policy, err := repoconfig.Parse([]byte("version: 1\nignore:\n  paths: [A.go]\n"))
	require.NoError(t, err)
	meta, err := json.Marshal(map[string]any{"repository_policy": map[string]any{"present": true, "digest": policy.Digest(), "config": policy.Config()}})
	require.NoError(t, err)
	rn, _ := g.Node(model.NodeID("r", model.KindRepository, "r"))
	rn.Meta = string(meta)
	require.NoError(t, db.ReplaceRepo(ctx, "r", g))
	page, err := svc.IdentityReviews(ctx, "r", "", 0, 20)
	require.NoError(t, err)
	assert.Equal(t, page.Total, 0)
	_, err = svc.ResolveSymbol(ctx, "r", in.From)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestReviewedIdentityChainPreservesEarlierReferences(t *testing.T) {
	ctx := context.Background()
	svc, db, in, g := reviewFixture(t)
	_, err := svc.ReviewIdentity(ctx, "r", agent, in)
	require.NoError(t, err)
	rn, _ := g.Node(model.NodeID("r", model.KindRepository, "r"))
	baseline, err := identity.Decode(rn.Identities, "r")
	require.NoError(t, err)
	next := model.NewGraph()
	next.AddNode(model.Node{ID: rn.ID, Repo: "r", Kind: model.KindRepository, Head: "three"})
	next.AddNode(model.Node{ID: model.NodeID("r", model.KindFunction, "C"), Repo: "r", Kind: model.KindFunction, Language: "go", Name: "C", File: "C.go", BodyHash: "C"})
	report, err := identity.Reconcile(next, "r", baseline)
	require.NoError(t, err)
	root, _ := next.Node(rn.ID)
	root.Identities, err = identity.Encode(report)
	require.NoError(t, err)
	require.NoError(t, db.ReplaceRepo(ctx, "r", next))
	current, _ := next.Node(model.NodeID("r", model.KindFunction, "C"))
	_, err = svc.ReviewIdentity(ctx, "r", agent, curation.IdentityReviewInput{From: in.To, To: current.SymbolID, Relation: "same-symbol", Reason: "Reviewed later rename", Revision: "three"})
	require.NoError(t, err)
	resolved, err := svc.ResolveSymbol(ctx, "r", in.From)
	require.NoError(t, err)
	assert.Equal(t, resolved.SymbolID, current.SymbolID)
	for _, s := range report.Symbols {
		if s.ID == in.From {
			assert.False(t, s.Active, "shared graph identity remains historical")
		}
	}
}
