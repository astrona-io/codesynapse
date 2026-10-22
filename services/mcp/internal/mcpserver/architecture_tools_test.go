package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/arch"
	"github.com/astrona-io/codesynapse/internal/catalog"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// archGraph indexes records on top of a one-package code graph.
func archGraph(t *testing.T, repo string, records map[string]string) *model.Graph {
	t.Helper()
	root := t.TempDir()
	for rel, text := range records {
		p := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(text), 0o600))
	}
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID(repo, model.KindRepository, repo), Kind: model.KindRepository, Repo: repo, Name: repo, Head: "abc", Time: "2026-10-22T10:00:00Z"})
	g.AddNode(model.Node{ID: model.NodeID(repo, model.KindPackage, "ex.com/app/store"), Kind: model.KindPackage, Repo: repo, Name: "store", QualifiedName: "ex.com/app/store"})
	g.AddNode(model.Node{ID: model.NodeID(repo, model.KindFunction, "ex.com/app/store.Open"), Kind: model.KindFunction, Repo: repo, Name: "Open",
		QualifiedName: "ex.com/app/store.Open", Package: "ex.com/app/store", File: "store/store.go", StartLine: 1, EndLine: 9, Exported: true})
	require.NoError(t, arch.Extract(context.Background(), repo, root, g))
	return g
}

var archRecords = map[string]string{
	"docs/adr/0001-old.md":  "# Old\n\n- Status: accepted\n- Topic: storage\n",
	"docs/adr/0002-new.md":  "---\nstatus: accepted\ntopic: storage\ncomponents: [store]\nsupersedes: ADR-0001\nimplements: [store.Open]\n---\n# New\n\n## Decision\n\nUse the new store everywhere.\n",
	"docs/adr/0003-idea.md": "# Idea\n\n- Status: proposed\n- Components: store\n",
}

func TestArchitectureTools(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	cs := connectRepos(t, map[string]*model.Graph{
		"org/a": archGraph(t, "org/a", archRecords),
		"org/b": archGraph(t, "org/b", map[string]string{"docs/adr/0001-secret.md": "# Secret\n\n- Status: accepted\n"}),
	})

	var page catalog.Page
	res := call(t, cs, "architecture_search", map[string]any{"repo": "org/a", "binding_only": true}, &page)
	require.False(t, res.IsError)
	require.Len(t, page.Entries, 1, "ADR-0001 is superseded and ADR-0003 is only proposed")
	assert.Equal(t, page.Entries[0].ID, "ADR-0002")
	assert.True(t, page.Entries[0].Binding)
	assert.Equal(t, page.Source.Head, "abc", "the answer says which revision it describes")

	// The tool and the web API are the same service: same filter, same answer.
	require.NoError(t, s.ReplaceRepo(ctx, "org/a", archGraph(t, "org/a", archRecords)))
	want, err := catalog.Service{R: store.ForRepository(s, "org/a")}.List(ctx, "org/a", catalog.Filter{BindingOnly: true, Limit: 20})
	require.NoError(t, err)
	a, _ := json.Marshal(want)
	b, _ := json.Marshal(page)
	assert.Equal(t, string(a), string(b))

	var d catalog.Detail
	res = call(t, cs, "architecture_get", map[string]any{"repo": "org/a", "id": "ADR-0001"}, &d)
	require.False(t, res.IsError)
	assert.False(t, d.Binding)
	require.Len(t, d.Chain, 1)
	assert.Equal(t, d.Chain[0].ID, "ADR-0002")
	assert.Equal(t, len(d.Problems), 1, "still accepted although replaced")

	var ctxRes catalog.ContextResult
	res = call(t, cs, "architecture_context", map[string]any{"repo": "org/a", "symbols": []string{"store.Open"}, "task": "open the store"}, &ctxRes)
	require.False(t, res.IsError)
	require.Len(t, ctxRes.Binding, 1)
	assert.Equal(t, ctxRes.Binding[0].ID, "ADR-0002")

	var issues architectureIssuesOut
	res = call(t, cs, "architecture_issues", map[string]any{"repo": "org/a"}, &issues)
	require.False(t, res.IsError)
	kinds := map[string]bool{}
	for _, is := range issues.Issues {
		kinds[is.Kind] = true
	}
	assert.True(t, kinds["supersession-mismatch"])

	// Errors are tool errors, not crashes; another repository is not reachable by ID.
	assert.True(t, call(t, cs, "architecture_get", map[string]any{"repo": "org/a", "id": "ADR-9999"}, nil).IsError)
	assert.True(t, call(t, cs, "architecture_get", map[string]any{"repo": "org/a"}, nil).IsError, "an id is required")
	res = call(t, cs, "architecture_search", map[string]any{"repo": "org/a", "query": "secret"}, &page)
	require.False(t, res.IsError)
	assert.Equal(t, page.Total, 0, "org/b's records do not appear in org/a's catalog")
}

func TestProposeAndLinkTools(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	require.NoError(t, s.ReplaceRepo(ctx, "org/a", archGraph(t, "org/a", archRecords)))
	ct, st := mcp.NewInMemoryTransports()
	_, err := newTestServer(t, s, "test", Options{Curated: s, CatalogWrites: true}).Connect(ctx, st, nil)
	require.NoError(t, err)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "claude-code"}, nil).Connect(ctx, ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })

	assert.Contains(t, cs.InitializeResult().Instructions, "never accepted", "agents are told a proposal is not a decision")
	tools, err := cs.ListTools(ctx, nil)
	require.NoError(t, err)
	for _, tool := range tools.Tools {
		if tool.Name == "architecture_propose" || tool.Name == "architecture_link" {
			assert.False(t, tool.Annotations.ReadOnlyHint, tool.Name+" writes, and says so")
		}
	}

	var p catalog.ProposalView
	res := call(t, cs, "architecture_propose", map[string]any{"repo": "org/a", "type": "adr", "title": "Use a queue", "body": "Jobs go through a queue.",
		"components": []string{"store"}, "supersedes": []string{"ADR-0003"}}, &p)
	require.False(t, res.IsError)
	assert.Equal(t, p.Status, "proposed")
	assert.False(t, p.Binding)
	assert.Equal(t, p.Client, "claude-code", "the proposal says which client made it")
	assert.Equal(t, p.SuggestedID, "ADR-0004")

	var listed proposalsOut
	require.False(t, call(t, cs, "architecture_proposals", map[string]any{"repo": "org/a"}, &listed).IsError)
	require.Len(t, listed.Proposals, 1)
	var full proposalsOut
	require.False(t, call(t, cs, "architecture_proposals", map[string]any{"repo": "org/a", "id": p.ID}, &full).IsError)
	assert.Contains(t, full.Proposals[0].Body, "Jobs go through a queue.")

	// Neither tool can approve or change a record: the catalog still shows ADR-0003 as proposed and the new one is not there.
	var page catalog.Page
	require.False(t, call(t, cs, "architecture_search", map[string]any{"repo": "org/a"}, &page).IsError)
	assert.Equal(t, page.Total, 3)
	assert.True(t, call(t, cs, "architecture_propose", map[string]any{"repo": "org/a", "type": "adr", "title": "x", "status": "accepted"}, nil).IsError, "there is no status to set")
	assert.True(t, call(t, cs, "architecture_propose", map[string]any{"repo": "org/a", "type": "adr", "title": "x", "supersedes": []string{"ADR-4242"}}, nil).IsError)

	var l catalog.LinkView
	res = call(t, cs, "architecture_link", map[string]any{"repo": "org/a", "kind": "implements", "record": "ADR-0002", "target": "store.Open"}, &l)
	require.False(t, res.IsError)
	assert.Equal(t, l.Client, "claude-code")
	var d catalog.Detail
	require.False(t, call(t, cs, "architecture_get", map[string]any{"repo": "org/a", "id": "ADR-0002"}, &d).IsError)
	linked := 0
	for _, c := range d.ImplementedBy {
		if c.Source == "link" {
			linked++
			assert.Equal(t, c.By, "claude-code")
		}
	}
	assert.Equal(t, linked, 0, "store.Open was already declared by the record, so the link adds no second entry")
	assert.Equal(t, len(d.Links), 1)
	assert.True(t, call(t, cs, "architecture_link", map[string]any{"repo": "org/a", "kind": "implements", "record": "ADR-0002", "target": "missing.Thing"}, nil).IsError)
}

func TestCatalogWritesAreOffWithoutTheFlag(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	require.NoError(t, s.ReplaceRepo(ctx, "org/a", archGraph(t, "org/a", archRecords)))
	ct, st := mcp.NewInMemoryTransports()
	_, err := newTestServer(t, s, "test", Options{Curated: s}).Connect(ctx, st, nil)
	require.NoError(t, err)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "x"}, nil).Connect(ctx, ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	assert.False(t, strings.Contains(cs.InitializeResult().Instructions, "architecture_propose"), "no write tools, no talk of them")
	tools, err := cs.ListTools(ctx, nil)
	require.NoError(t, err)
	names := map[string]bool{}
	for _, tool := range tools.Tools {
		names[tool.Name] = true
	}
	assert.True(t, names["architecture_proposals"], "reading proposals stays on")
	assert.False(t, names["architecture_propose"])
	assert.False(t, names["architecture_link"])
	assert.False(t, names["code_annotate"])
	assert.False(t, names["code_record_reuse"])
}

func TestCatalogWritesNeedEditorAccessAndAreAudited(t *testing.T) {
	url, keys, acc, _, db := keyServer(t)
	ctx := context.Background()
	require.NoError(t, db.ReplaceRepo(ctx, storetest.Repo, archGraph(t, storetest.Repo, archRecords)))
	owner, err := acc.AuthenticateKey(ctx, keys["a"])
	require.NoError(t, err)
	cs, err := connectKey(t, url, keys["a"])
	require.NoError(t, err)

	var p catalog.ProposalView
	res := callContext(ctx, t, cs, "architecture_propose", map[string]any{"repo": storetest.Repo, "type": "adr", "title": "Use a queue"}, &p)
	require.False(t, res.IsError)
	assert.Equal(t, p.CreatedBy, owner.User.ID, "the proposal records the user behind the key")

	events, err := acc.St.ListEvents(ctx, accounts.EventFilter{})
	require.NoError(t, err)
	audited := false
	for _, e := range events {
		audited = audited || (e.Action == "mcp.architecture.propose" && e.Target == storetest.Repo && e.UserID == owner.User.ID && e.Outcome == accounts.Success)
	}
	assert.True(t, audited, "the change is in the audit log")

	// Another organisation that shares the graph does not see the proposal.
	other, err := connectKey(t, url, keys["b"], "chat-b")
	require.NoError(t, err)
	var seen proposalsOut
	require.False(t, callContext(ctx, t, other, "architecture_proposals", map[string]any{"repo": storetest.Repo}, &seen).IsError)
	assert.Len(t, seen.Proposals, 0)

	// Reading is allowed at viewer level, writing is not.
	actor := accounts.Principal{User: owner.User, Via: "session"}
	backup, _, err := acc.SignUp(ctx, "backup@example.com", "Backup", "correct horse battery staple")
	require.NoError(t, err)
	_, err = acc.AddMember(ctx, actor, owner.OrgID, backup.Email, accounts.Member)
	require.NoError(t, err)
	_, err = acc.SetRepoGrant(ctx, actor, owner.OrgID, storetest.Repo, accounts.Tuple{SubjectType: accounts.TypeUser, SubjectID: backup.ID}, accounts.Admin)
	require.NoError(t, err)
	_, err = acc.SetRepoGrant(ctx, actor, owner.OrgID, storetest.Repo, accounts.Tuple{SubjectType: accounts.TypeUser, SubjectID: owner.User.ID}, accounts.Viewer)
	require.NoError(t, err)
	assert.False(t, callContext(ctx, t, cs, "architecture_search", map[string]any{"repo": storetest.Repo}, nil).IsError)
	assert.True(t, callContext(ctx, t, cs, "architecture_propose", map[string]any{"repo": storetest.Repo, "type": "adr", "title": "Denied"}, nil).IsError)
	assert.True(t, callContext(ctx, t, cs, "architecture_link", map[string]any{"repo": storetest.Repo, "kind": "relates", "record": "ADR-0001", "target": "ADR-0002"}, nil).IsError)
	assert.True(t, callContext(ctx, t, cs, "code_annotate", map[string]any{"repo": storetest.Repo, "symbol": "sym-00000000000000000000000000000000", "purpose": "Denied"}, nil).IsError)
	assert.True(t, callContext(ctx, t, cs, "code_record_reuse", map[string]any{"repo": storetest.Repo, "method": "copy", "origins": []string{}, "destinations": []string{}, "reason": "Denied", "compatibility": "unknown", "license": "unknown", "attribution": "unknown", "validation": "not run"}, nil).IsError)
}
