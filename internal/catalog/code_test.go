package catalog

import (
	"context"
	"strings"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/curation"
	"github.com/parisnakitakejser/codesynapse/internal/identity"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/query"
	"github.com/parisnakitakejser/codesynapse/internal/store"
	"github.com/parisnakitakejser/codesynapse/internal/store/memory"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func codeGraph(t *testing.T, previous identity.Report, revision, body string) (*model.Graph, identity.Report) {
	t.Helper()
	g := model.NewGraph()
	rn := g.AddNode(model.Node{ID: model.NodeID("r", model.KindRepository, "r"), Repo: "r", Kind: model.KindRepository, Head: revision})
	for _, name := range []string{"A", "B"} {
		g.AddNode(model.Node{ID: model.NodeID("r", model.KindFunction, name), Repo: "r", Kind: model.KindFunction, Name: name, File: name + ".go", BodyHash: body + name, Language: "go"})
	}
	r, err := identity.Reconcile(g, "r", previous)
	require.NoError(t, err)
	rn.Identities, err = identity.Encode(r)
	require.NoError(t, err)
	return g, r
}

func TestCodeRecordsScopeAndDivergence(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	g, baseline := codeGraph(t, identity.Report{Version: 1, Repo: "r"}, "rev1", "old")
	require.NoError(t, st.ReplaceRepo(ctx, "r", g))
	svc := Service{R: store.ForRepository(st, "r"), Items: st, Org: agent.Org, Now: fixedNow}
	a, b := baseline.Symbols[0].ID, baseline.Symbols[1].ID
	input := curation.CodeInput{Method: "copy", Origins: []string{a}, Destinations: []string{b}, Reason: "existing implementation fits", Compatibility: "checked API", License: "MIT, attribution retained", Attribution: "original author", Validation: "go test ./..."}
	v, err := svc.RecordCode(ctx, "r", agent, input)
	require.NoError(t, err)
	assert.Equal(t, v.Freshness, "current")
	assert.Equal(t, v.Item.Code.Sources[0].Revision, "rev1")
	assert.Equal(t, v.Item.CreatedBy, "u1")
	assert.Equal(t, v.Item.OrgID, "o1")
	_, err = svc.RecordCode(ctx, "r", agent, curation.CodeInput{Method: "annotation", Destinations: []string{a}, Purpose: "Token ghp_abcdefghijklmnopqrstuvwxyz0123456789", Labels: []string{"Storage"}})
	require.NoError(t, err)
	page, err := svc.CodeRecords(ctx, "r", a, 0, 20)
	require.NoError(t, err)
	assert.Len(t, page.Records, 2)
	for _, record := range page.Records {
		if record.Item.Code.Method == "annotation" {
			assert.False(t, strings.Contains(record.Item.Code.Purpose, "ghp_abcdefghijklmnopqrstuvwxyz0123456789"))
			assert.Equal(t, record.Item.Code.Labels, []string{"storage"})
		}
	}
	other := svc
	other.Org = "o2"
	page, err = other.CodeRecords(ctx, "r", "", 0, 20)
	require.NoError(t, err)
	assert.Len(t, page.Records, 0)
	_, err = other.RecordCode(ctx, "r", agent, input)
	assert.ErrorIs(t, err, curation.ErrInvalid)
	_, err = svc.RecordCode(ctx, "r", agent, curation.CodeInput{Method: "annotation", Destinations: []string{"sym-00000000000000000000000000000000"}, Purpose: "secret"})
	assert.ErrorIs(t, err, curation.ErrInvalid)
	_, err = (query.Service{R: store.ForRepository(st, "other")}).Identities(ctx, "r", a, false, 0, 20)
	assert.ErrorIs(t, err, store.ErrNotFound)
	newGraph, _ := codeGraph(t, baseline, "rev2", "changed")
	require.NoError(t, st.ReplaceRepo(ctx, "r", newGraph))
	page, err = svc.CodeRecords(ctx, "r", b, 0, 20)
	require.NoError(t, err)
	require.Len(t, page.Records, 1)
	assert.Equal(t, page.Records[0].Freshness, "needs review")
	assert.Len(t, page.Records[0].Changed, 2)
	assert.Equal(t, page.Records[0].Item.Code.Sources[0].Revision, "rev1", "historical evidence never rewritten")
	n, err := (query.Service{R: st}).Resolve(ctx, "r", a)
	require.NoError(t, err)
	assert.Equal(t, n.BodyHash, "changedA")
	ids, err := (query.Service{R: st}).Identities(ctx, "r", "", false, 0, 1)
	require.NoError(t, err)
	assert.True(t, ids.Truncated)
	assert.Equal(t, ids.Total, 2)
	assert.Equal(t, ids.Omitted, 1)
}
