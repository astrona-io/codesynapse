package catalog

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/curation"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

var fixedNow = func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) }

func withItems(svc Service, st *memory.Store, org string) Service {
	svc.Items, svc.Org, svc.Now = st, org, fixedNow
	return svc
}

var agent = Actor{Org: "o1", User: "u1", Client: "claude-code"}

func TestProposeIsADraftNeverARule(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	svc := withItems(catalogOf(t, "r", records, st), st, "o1")

	p, err := svc.Propose(ctx, "r", agent, curation.ProposalInput{
		Type: "ADR", Title: "Use a read replica for reports", Body: "Reports read from a replica.\nToken ghp_abcdefghijklmnopqrstuvwxyz0123456789 must not leak.",
		Topic: "Storage", Components: []string{"Store", "api"}, Supersedes: []string{"adr-0001"}, Relates: []string{"ADR-0002"}, Implements: []string{"store.Open"},
	})
	require.NoError(t, err)
	assert.Equal(t, p.Type, "adr")
	assert.Equal(t, p.Status, "proposed", "an ADR proposal starts as proposed")
	assert.Equal(t, p.State, curation.Open)
	assert.False(t, p.Binding)
	assert.Equal(t, p.SuggestedID, "ADR-0007", "the next number after ADR-0006")
	assert.Equal(t, p.SuggestedPath, "docs/adr/0007-use-a-read-replica-for-reports.md")
	assert.Equal(t, p.Supersedes, []string{"ADR-0001"}, "IDs are written the way the record spells them")
	assert.Equal(t, p.Components, []string{"api", "store"})
	assert.Equal(t, p.Client, "claude-code")
	assert.Contains(t, p.Notes[0], "not a decision")
	require.Len(t, p.Implements, 1)
	assert.True(t, p.Implements[0].Found)
	assert.False(t, strings.Contains(p.Body, "ghp_abcdefghijklmnopqrstuvwxyz0123456789"), "secrets are redacted: %s", p.Body)

	// It is not a record: not in the list, not a rule, not in force for a task.
	all, err := svc.List(ctx, "r", Filter{})
	require.NoError(t, err)
	assert.Equal(t, all.Total, 9)
	res, err := svc.Context(ctx, "r", Task{Symbols: []string{"store.Open"}, Query: "reports replica"})
	require.NoError(t, err)
	for _, a := range res.Binding {
		assert.True(t, !strings.Contains(a.Title, "replica"))
	}
	require.Len(t, res.OpenProposals, 1, "but the work is told that this proposal exists")
	assert.Equal(t, res.OpenProposals[0].Title, "Use a read replica for reports")

	// RFCs start as drafts; types that are written as files are refused.
	rfc, err := svc.Propose(ctx, "r", agent, curation.ProposalInput{Type: "rfc", Title: "Sharding v2"})
	require.NoError(t, err)
	assert.Equal(t, rfc.Status, "draft")
	assert.Equal(t, rfc.SuggestedID, "RFC-0008")
	_, err = svc.Propose(ctx, "r", agent, curation.ProposalInput{Type: "overview", Title: "x"})
	assert.ErrorIs(t, err, curation.ErrInvalid)
	_, err = svc.Propose(ctx, "r", agent, curation.ProposalInput{Type: "adr", Title: ""})
	assert.ErrorIs(t, err, curation.ErrInvalid)
	_, err = svc.Propose(ctx, "r", agent, curation.ProposalInput{Type: "adr", Title: "x", Supersedes: []string{"ADR-4242"}})
	assert.ErrorIs(t, err, curation.ErrInvalid, "it cannot supersede a record that does not exist")
	_, err = svc.Propose(ctx, "r", agent, curation.ProposalInput{Type: "adr", Title: "x", Implements: []string{"nowhere.Missing"}})
	assert.ErrorIs(t, err, curation.ErrInvalid, "code references must name a place")
	_, err = svc.Propose(ctx, "r", agent, curation.ProposalInput{Type: "adr", Title: "x", Components: []string{"Not Valid!"}})
	assert.ErrorIs(t, err, curation.ErrInvalid)

	_, err = catalogOf(t, "r2", records, memory.New()).Propose(ctx, "r2", agent, curation.ProposalInput{Type: "adr", Title: "x"})
	assert.ErrorIs(t, err, ErrDisabled, "without somewhere to keep it there is no proposing")
}

func TestExportMergeAndWithdraw(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	svc := withItems(catalogOf(t, "r", records, st), st, "o1")
	p, err := svc.Propose(ctx, "r", agent, curation.ProposalInput{Type: "adr", Title: "Edge caching", Body: "Cache at the edge.", Components: []string{"api"}, Supersedes: []string{"ADR-0003"}})
	require.NoError(t, err)

	path, md, err := svc.Export(ctx, "r", p.ID)
	require.NoError(t, err)
	assert.Equal(t, path, "docs/adr/0007-edge-caching.md")
	assert.True(t, strings.HasPrefix(md, "---\ntype: adr\nstatus: proposed\n"), md)
	assert.Contains(t, md, "components: [api]")
	assert.Contains(t, md, "supersedes: [ADR-0003]")
	assert.Contains(t, md, "# Edge caching\n\nCache at the edge.")
	assert.Contains(t, md, "not a decision until merged with an accepted status")

	// Once the file is in the repository the proposal is shown as merged.
	merged := map[string]string{}
	for k, v := range records {
		merged[k] = v
	}
	merged[path] = md
	svc2 := withItems(catalogOf(t, "r", merged, st), st, "o1")
	list, err := svc2.Proposals(ctx, "r")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, list[0].State, "merged")
	assert.Equal(t, list[0].SuggestedID, "ADR-0007")

	// Withdrawing keeps it as history and takes it out of the work.
	st2 := memory.New()
	svc3 := withItems(catalogOf(t, "r", records, st2), st2, "o1")
	p2, err := svc3.Propose(ctx, "r", agent, curation.ProposalInput{Type: "adr", Title: "Edge caching"})
	require.NoError(t, err)
	w, err := svc3.Withdraw(ctx, "r", p2.ID)
	require.NoError(t, err)
	assert.Equal(t, w.State, curation.Withdrawn)
	res, err := svc3.Context(ctx, "r", Task{Query: "edge caching"})
	require.NoError(t, err)
	assert.Equal(t, len(res.OpenProposals), 0)
	all, _ := svc3.Proposals(ctx, "r")
	assert.Len(t, all, 1, "history is kept")
	_, _, err = svc3.Export(ctx, "r", "nope")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestProposalsAreKeptApartPerOrganisation(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	base := catalogOf(t, "r", records, st)
	one, two := withItems(base, st, "o1"), withItems(base, st, "o2")
	p, err := one.Propose(ctx, "r", agent, curation.ProposalInput{Type: "adr", Title: "Private plan"})
	require.NoError(t, err)
	l, err := one.AddLink(ctx, "r", agent, "implements", "ADR-0002", "store.Open", "")
	require.NoError(t, err)

	seen, err := two.Proposals(ctx, "r")
	require.NoError(t, err)
	assert.Len(t, seen, 0, "another organisation sharing the graph sees nothing")
	_, err = two.Proposal(ctx, "r", p.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = two.Withdraw(ctx, "r", p.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.ErrorIs(t, two.RemoveLink(ctx, "r", l.ID), store.ErrNotFound)
	links, err := two.Links(ctx, "r", "")
	require.NoError(t, err)
	assert.Len(t, links, 0)
	d, err := two.Get(ctx, "r", "ADR-0002")
	require.NoError(t, err)
	for _, c := range d.ImplementedBy {
		assert.Equal(t, c.Source, "record", "links of another organisation are not shown")
	}
}

func TestLinksTieRecordsToCodeAndStayHonest(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	svc := withItems(catalogOf(t, "r", records, st), st, "o1")

	l, err := svc.AddLink(ctx, "r", agent, "implements", "adr-0004", "store.Open", "logging goes through the store")
	require.NoError(t, err)
	assert.Equal(t, l.Record, "ADR-0004")
	assert.Equal(t, l.Client, "claude-code")
	require.Len(t, l.Resolved, 1)
	assert.Equal(t, l.Resolved[0].Name, "store.Open")
	again, err := svc.AddLink(ctx, "r", agent, "implements", "ADR-0004", "STORE.OPEN", "")
	require.NoError(t, err)
	assert.Equal(t, again.ID, l.ID, "the same link twice is one link")

	_, err = svc.AddLink(ctx, "r", agent, "implements", "ADR-4242", "store.Open", "")
	assert.ErrorIs(t, err, curation.ErrInvalid, "unknown record")
	_, err = svc.AddLink(ctx, "r", agent, "implements", "ADR-0004", "missing.Thing", "")
	assert.ErrorIs(t, err, curation.ErrInvalid, "unknown code")
	_, err = svc.AddLink(ctx, "r", agent, "implements", "ADR-0004", "Reader", "")
	assert.NoError(t, err, "a unique bare name is fine")
	_, err = svc.AddLink(ctx, "r", agent, "relates", "ADR-0004", "ADR-0004", "")
	assert.ErrorIs(t, err, curation.ErrInvalid, "a record does not relate to itself")
	_, err = svc.AddLink(ctx, "r", agent, "owns", "ADR-0004", "x", "")
	assert.ErrorIs(t, err, curation.ErrInvalid)

	d, err := svc.Get(ctx, "r", "ADR-0004")
	require.NoError(t, err)
	var linked []CodeLink
	for _, c := range d.ImplementedBy {
		if c.Source == "link" {
			linked = append(linked, c)
		}
	}
	require.Len(t, linked, 2)
	assert.Equal(t, linked[0].By, "claude-code", "a person can tell an agent's link from a declared one")
	assert.Equal(t, len(d.Links), 2)

	// The link counts for a task that touches the code, and says why.
	res, err := svc.Context(ctx, "r", Task{Symbols: []string{"store.Open"}})
	require.NoError(t, err)
	found := false
	for _, a := range res.Binding {
		if a.ID == "ADR-0004" {
			found = true
			assert.Contains(t, strings.Join(a.Why, " "), "linked by claude-code to store.Open")
		}
	}
	assert.True(t, found, "ADR-0004 is a rule in force and its linked code is touched")

	// relates works in both directions; applies-to adds a component to filter by.
	_, err = svc.AddLink(ctx, "r", agent, "relates", "ADR-0003", "ADR-0006", "")
	require.NoError(t, err)
	other, err := svc.Get(ctx, "r", "ADR-0006")
	require.NoError(t, err)
	require.Len(t, other.Related, 1)
	assert.Equal(t, other.Related[0].ID, "ADR-0003")
	_, err = svc.AddLink(ctx, "r", agent, "applies-to", "ADR-0006", "Billing", "")
	require.NoError(t, err)
	comp, err := svc.List(ctx, "r", Filter{Component: "billing"})
	require.NoError(t, err)
	require.Equal(t, comp.Total, 1)
	assert.Equal(t, comp.Entries[0].ID, "ADR-0006")

	require.NoError(t, svc.RemoveLink(ctx, "r", l.ID))
	assert.ErrorIs(t, svc.RemoveLink(ctx, "r", l.ID), store.ErrNotFound)
	links, err := svc.Links(ctx, "r", "ADR-0004")
	require.NoError(t, err)
	assert.Len(t, links, 1)
}

func TestLinkGoesStaleInsteadOfVanishing(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	first := withItems(catalogOf(t, "r", records, st), st, "o1")
	_, err := first.AddLink(ctx, "r", agent, "implements", "ADR-0004", "store.Open", "")
	require.NoError(t, err)

	// The next index no longer has store.Open (it was renamed).
	svc := withItems(indexWith(t, "r", records, st, "r|Function|ex.com/app/internal/store.Open"), st, "o1")

	d, err := svc.Get(ctx, "r", "ADR-0004")
	require.NoError(t, err)
	require.Len(t, d.ImplementedBy, 1)
	assert.True(t, d.ImplementedBy[0].Stale)
	assert.Contains(t, d.ImplementedBy[0].Problem, "nothing in the code is called")
	issues, _, err := svc.Issues(ctx, "r")
	require.NoError(t, err)
	kinds := map[string]bool{}
	for _, i := range issues {
		kinds[i.Kind] = true
	}
	assert.True(t, kinds["stale-link"])
}
