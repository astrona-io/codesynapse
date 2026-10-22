package catalog

import (
	"context"
	"encoding/json"
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

func codeGraph(t *testing.T, repo string, previous identity.Report, revision, body string) (*model.Graph, identity.Report) {
	t.Helper()
	g := model.NewGraph()
	rn := g.AddNode(model.Node{ID: model.NodeID(repo, model.KindRepository, repo), Repo: repo, Kind: model.KindRepository, Head: revision})
	for _, name := range []string{"A", "B"} {
		g.AddNode(model.Node{ID: model.NodeID(repo, model.KindFunction, name), Repo: repo, Kind: model.KindFunction, Name: name, File: name + ".go", BodyHash: body + name, Language: "go"})
	}
	r, err := identity.Reconcile(g, repo, previous)
	require.NoError(t, err)
	rn.Identities, err = identity.Encode(r)
	require.NoError(t, err)
	return g, r
}

func TestCodeRecordsScopeAndDivergence(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	g, baseline := codeGraph(t, "r", identity.Report{Version: 1, Repo: "r"}, "rev1", "old")
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
	newGraph, _ := codeGraph(t, "r", baseline, "rev2", "changed")
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

func TestCrossRepositoryProvenanceRechecksSource(t *testing.T) {
	ctx := context.Background()
	db := memory.New()
	g, destination := codeGraph(t, "r", identity.Report{Version: 1, Repo: "r"}, "dest-rev", "dest")
	src, source := codeGraph(t, "private/source", identity.Report{Version: 1, Repo: "private/source"}, "source-rev", "secret")
	require.NoError(t, db.ReplaceRepo(ctx, "r", g))
	require.NoError(t, db.ReplaceRepo(ctx, "private/source", src))
	svc := Service{R: store.ForRepository(db, "r"), Items: db, Org: agent.Org, Now: fixedNow}
	in := curation.CodeInput{Method: "adapt", OriginRepo: "private/source", Origins: []string{source.Symbols[0].ID}, Destinations: []string{destination.Symbols[0].ID}, Reason: "sensitive source rationale", Compatibility: "checked", License: "MIT", Attribution: "source author", Validation: "tests passed"}
	_, err := svc.RecordCode(ctx, "r", agent, in)
	assert.ErrorIs(t, err, store.ErrNotFound, "cross-repository reads require an explicit authorizer even with an unrestricted backing store")
	allowed := true
	svc.ReadRepository = func(_ context.Context, repo string) (store.Reader, error) {
		if !allowed || repo != "private/source" {
			return nil, store.ErrNotFound
		}
		return store.ForRepository(db, repo), nil
	}
	v, err := svc.RecordCode(ctx, "r", agent, in)
	require.NoError(t, err)
	assert.Equal(t, v.Freshness, "current")
	assert.Equal(t, v.Item.Code.Sources[0].Repo, "private/source")
	assert.Equal(t, v.Item.Code.Sources[0].Revision, "source-rev")
	assert.Equal(t, v.Item.Code.Targets[0].Repo, "r")
	allowed = false
	page, err := svc.CodeRecords(ctx, "r", "", 0, 20)
	require.NoError(t, err)
	assert.True(t, page.SourcesRestricted)
	assert.Len(t, page.Records, 0)
	assert.Equal(t, page.Total, 0)
	serialized, err := json.Marshal(page)
	require.NoError(t, err)
	for _, secret := range []string{"private/source", in.Reason, in.Origins[0], "source-rev"} {
		assert.False(t, strings.Contains(string(serialized), secret), "restricted record leaked %s", secret)
	}
	_, err = svc.RecordCode(ctx, "r", agent, in)
	assert.ErrorIs(t, err, store.ErrNotFound)
	stored, err := db.ListCurated(ctx, "r", curation.KindCode, 20)
	require.NoError(t, err)
	assert.Len(t, stored, 1)
	allowed = true
	updated, _ := codeGraph(t, "private/source", source, "new-source-rev", "edited")
	require.NoError(t, db.ReplaceRepo(ctx, "private/source", updated))
	page, err = svc.CodeRecords(ctx, "r", "", 0, 20)
	require.NoError(t, err)
	require.Len(t, page.Records, 1)
	assert.Equal(t, page.Records[0].Freshness, "needs review")
	assert.Equal(t, page.Records[0].Item.Code.Sources[0].Revision, "source-rev")
	require.NoError(t, db.DeleteRepo(ctx, "private/source"))
	page, err = svc.CodeRecords(ctx, "r", "", 0, 20)
	require.NoError(t, err)
	require.Len(t, page.Records, 1)
	assert.True(t, page.Records[0].SourceUnavailable)
	assert.Equal(t, page.Records[0].Freshness, "needs review")
}
