package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/accounts"
	acctmem "github.com/parisnakitakejser/codesynapse/internal/accounts/memstore"
	"github.com/parisnakitakejser/codesynapse/internal/arch"
	"github.com/parisnakitakejser/codesynapse/internal/catalog"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/store/memory"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func recordsGraph(t *testing.T, repo string, files map[string]string) *model.Graph {
	t.Helper()
	root := t.TempDir()
	for rel, text := range files {
		p := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(text), 0o600))
	}
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID(repo, model.KindRepository, repo), Kind: model.KindRepository, Repo: repo, Name: repo, Head: "abc", Time: "2026-10-22T10:00:00Z"})
	require.NoError(t, arch.Extract(context.Background(), repo, root, g))
	return g
}

func TestArchitectureEndpoints(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, "org/a", recordsGraph(t, "org/a", map[string]string{
		"docs/adr/0001-a.md": "# A\n\n- Status: accepted\n- Topic: db\n- Components: api\n",
		"docs/adr/0002-b.md": "# B\n\n- Status: accepted\n- Topic: db\n- Components: api\n",
		"docs/adr/0003-c.md": "# C\n\n- Status: proposed\n",
	})))
	require.NoError(t, st.ReplaceRepo(ctx, "org/b", recordsGraph(t, "org/b", map[string]string{"docs/adr/0001-x.md": "# Hidden\n\n- Status: accepted\n"})))
	ts := httptest.NewServer(New(ctx, Config{SourcesRoot: t.TempDir(), Version: "test"}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)
	base := "/api/v1/repos/org%2Fa/architecture"

	var page catalog.Page
	require.Equal(t, get(t, ts, base, &page), 200)
	assert.Equal(t, page.Total, 3)
	assert.Equal(t, page.Source.Head, "abc")
	require.Equal(t, get(t, ts, base+"?binding=1&component=API", &page), 200)
	assert.Equal(t, page.Total, 2)
	require.Equal(t, get(t, ts, base+"?status=proposed", &page), 200)
	assert.Equal(t, page.Entries[0].ID, "ADR-0003")
	assert.False(t, page.Entries[0].Binding, "a proposal is not a rule")
	require.Equal(t, get(t, ts, base+"?limit=1&offset=1", &page), 200)
	assert.Equal(t, len(page.Entries), 1)
	assert.Equal(t, page.Total, 3)

	var d catalog.Detail
	require.Equal(t, get(t, ts, base+"/record?id=ADR-0002", &d), 200)
	assert.Equal(t, d.Title, "B")
	assert.Equal(t, get(t, ts, base+"/record?id=ADR-0099", nil), 404)
	assert.Equal(t, get(t, ts, base+"/record", nil), 400)
	assert.Equal(t, get(t, ts, base+"/record?id=ADR-0001", nil), 200)

	var issues struct {
		Issues []catalog.Issue `json:"issues"`
	}
	require.Equal(t, get(t, ts, base+"/issues", &issues), 200)
	require.Len(t, issues.Issues, 1)
	assert.Equal(t, issues.Issues[0].Kind, "potential-conflict")
	assert.Equal(t, issues.Issues[0].Records, []string{"ADR-0001", "ADR-0002"})

	var res catalog.ContextResult
	require.Equal(t, get(t, ts, base+"/context?components=api", &res), 200)
	assert.Equal(t, len(res.Binding), 2)
	assert.Equal(t, len(res.Issues), 1, "the possible conflict comes with the rules, unresolved")

	// Another repository's records are not in this catalog, and an unknown repository is a 404.
	require.Equal(t, get(t, ts, base+"?q=hidden", &page), 200)
	assert.Equal(t, page.Total, 0)
	assert.Equal(t, get(t, ts, "/api/v1/repos/nope/architecture", nil), 404)
}

func TestProposalsAndLinksOverHTTP(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	g := recordsGraph(t, "org/a", map[string]string{
		"docs/adr/0001-a.md": "# A\n\n- Status: accepted\n- Components: api\n",
		"docs/adr/0002-b.md": "# B\n\n- Status: accepted\n",
	})
	g.AddNode(model.Node{ID: "org/a|Function|ex.com/p.Open", Kind: model.KindFunction, Repo: "org/a", Name: "Open", QualifiedName: "ex.com/p.Open", Package: "ex.com/p", File: "p/p.go", StartLine: 1, EndLine: 3, Exported: true})
	require.NoError(t, st.ReplaceRepo(ctx, "org/a", g))
	ts := httptest.NewServer(New(ctx, Config{SourcesRoot: t.TempDir(), Version: "test"}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)
	c := newClient(t, ts)
	base := "/api/v1/repos/org%2Fa/architecture"

	var p catalog.ProposalView
	require.Equal(t, c.do("POST", base+"/proposals", `{"type":"adr","title":"Use a queue","body":"Jobs go through a queue.","components":["api"],"supersedes":["ADR-0002"],"implements":["Open"]}`, &p), 201)
	assert.Equal(t, p.Status, "proposed")
	assert.False(t, p.Binding)
	assert.Equal(t, p.SuggestedID, "ADR-0003")
	assert.Equal(t, p.Client, "web")

	var list struct {
		Proposals []catalog.ProposalView `json:"proposals"`
	}
	require.Equal(t, c.do("GET", base+"/proposals", "", &list), 200)
	require.Len(t, list.Proposals, 1)
	assert.Equal(t, list.Proposals[0].Body, "", "the list leaves the text out")
	var one catalog.ProposalView
	require.Equal(t, c.do("GET", base+"/proposals/"+p.ID, "", &one), 200)
	assert.Contains(t, one.Body, "Jobs go through a queue.")

	var md string
	require.Equal(t, c.do("GET", base+"/proposals/"+p.ID+"/export", "", &md), 200)
	assert.Contains(t, md, "status: proposed")
	assert.Contains(t, md, "supersedes: [ADR-0002]")
	res, err := http.Get(ts.URL + base + "/proposals/" + p.ID + "/export") //nolint:noctx // test
	require.NoError(t, err)
	defer res.Body.Close() //nolint:errcheck // test
	assert.Equal(t, res.Header.Get("Content-Disposition"), `attachment; filename="0003-use-a-queue.md"`)
	assert.Equal(t, res.Header.Get("X-Suggested-Path"), "docs/adr/0003-use-a-queue.md")

	// Bad input is a 400, never a stored proposal.
	assert.Equal(t, c.do("POST", base+"/proposals", `{"type":"overview","title":"x"}`, nil), 400)
	assert.Equal(t, c.do("POST", base+"/proposals", `{"type":"adr","title":"x","approved":true}`, nil), 400, "there is no way to say it is approved")
	assert.Equal(t, c.do("POST", base+"/proposals", `{"type":"adr","title":"x","supersedes":["ADR-4242"]}`, nil), 400)
	assert.Equal(t, c.do("POST", base+"/proposals", `{`, nil), 400)
	assert.Equal(t, c.do("POST", base+"/proposals", `{"type":"adr","title":"`+strings.Repeat("x", 70<<10)+`"}`, nil), 400, "oversized bodies are refused")

	// The catalog itself is untouched: a proposal is not a record.
	var page catalog.Page
	require.Equal(t, c.do("GET", base, "", &page), 200)
	assert.Equal(t, page.Total, 2)

	var l catalog.LinkView
	require.Equal(t, c.do("POST", base+"/links", `{"kind":"implements","record":"adr-0001","target":"Open","note":"queue entry point"}`, &l), 201)
	assert.Equal(t, l.Record, "ADR-0001")
	require.Len(t, l.Resolved, 1)
	assert.Equal(t, c.do("POST", base+"/links", `{"kind":"implements","record":"ADR-0001","target":"Nope"}`, nil), 400)
	var d catalog.Detail
	require.Equal(t, c.do("GET", base+"/record?id=ADR-0001", "", &d), 200)
	require.Len(t, d.Links, 1)
	require.Equal(t, c.do("GET", base+"/links?record=ADR-0001", "", &struct{}{}), 200)
	assert.Equal(t, c.do("DELETE", base+"/links/"+l.ID, "", nil), 204)
	assert.Equal(t, c.do("DELETE", base+"/links/"+l.ID, "", nil), 404)

	var w catalog.ProposalView
	require.Equal(t, c.do("POST", base+"/proposals/"+p.ID+"/withdraw", "", &w), 200)
	assert.Equal(t, w.State, "withdrawn")
	assert.Equal(t, c.do("POST", base+"/proposals/nope/withdraw", "", nil), 404)
}

func TestCatalogWritesNeedTheEditorRole(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, "acme/web", recordsGraph(t, "acme/web", map[string]string{"docs/adr/0001-a.md": "# A\n\n- Status: accepted\n"})))
	acc := accounts.NewService(acctmem.New(), true)
	ts := httptest.NewServer(New(ctx, Config{Accounts: acc}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)
	signup := func(email string) *client {
		c := newClient(t, ts)
		require.Equal(t, c.do("POST", "/api/v1/auth/signup", `{"email":"`+email+`","name":"`+email+`","password":"`+pw+`"}`, nil), 201)
		return c
	}
	_ = signup("ada@example.com") // platform admin
	bob, carol, dave := signup("bob@example.com"), signup("carol@example.com"), signup("dave@example.com")
	var acme, ext accounts.Org
	require.Equal(t, bob.do("POST", "/api/v1/accounts/orgs", `{"name":"Acme"}`, &acme), 201)
	require.Equal(t, dave.do("POST", "/api/v1/accounts/orgs", `{"name":"Ext"}`, &ext), 201)
	bobUser, err := acc.St.UserByEmail(ctx, "bob@example.com")
	require.NoError(t, err)
	require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: acme.ID, Graph: "acme/web", AddedBy: bobUser.ID, AddedAt: time.Now()}))
	require.Equal(t, bob.do("PUT", "/api/v1/accounts/orgs/"+acme.Slug+"/members", `{"email":"carol@example.com","role":"guest"}`, nil), 200)
	require.Equal(t, bob.do("PUT", "/api/v1/accounts/orgs/"+acme.Slug+"/repos/"+url.PathEscape("acme/web")+"/access", `{"user":"carol@example.com","role":"viewer"}`, nil), 200)
	inAcme, inExt := []string{OrgHeader, acme.Slug}, []string{OrgHeader, ext.Slug}
	base := "/api/v1/repos/acme%2Fweb/architecture"
	body := `{"type":"adr","title":"Use a queue"}`

	assert.Equal(t, carol.do("GET", base, "", nil, inAcme...), 200, "a viewer reads the catalog")
	assert.Equal(t, carol.do("POST", base+"/proposals", body, nil, inAcme...), 403, "but cannot propose")
	assert.Equal(t, carol.do("POST", base+"/links", `{"kind":"relates","record":"ADR-0001","target":"ADR-0001"}`, nil, inAcme...), 403)
	assert.Equal(t, dave.do("GET", base, "", nil, inExt...), 404, "another organisation does not see the repository")
	assert.Equal(t, dave.do("POST", base+"/proposals", body, nil, inExt...), 404)

	var p catalog.ProposalView
	require.Equal(t, bob.do("POST", base+"/proposals", body, &p, inAcme...), 201)
	bobID := bobUser.ID
	assert.Equal(t, p.CreatedBy, bobID, "who proposed it is recorded")
	var list struct {
		Proposals []catalog.ProposalView `json:"proposals"`
	}
	require.Equal(t, carol.do("GET", base+"/proposals", "", &list, inAcme...), 200)
	assert.Len(t, list.Proposals, 1, "colleagues of the organisation see it")
	assert.Equal(t, carol.do("POST", base+"/proposals/"+p.ID+"/withdraw", "", nil, inAcme...), 403)
}
