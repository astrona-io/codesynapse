package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

// TestRecallLean: a recall answer carries names, predicates and types, not
// the namespaced IDs, repository keys and timestamps it is stored with.
func TestRecallLean(t *testing.T) {
	ns := "session:" + strings.Repeat("ab", 32)
	id := func(n string) string { return ns + memorySep + n }
	in := recallOut{
		Mode: "hybrid",
		Entries: []recallEntry{{
			Entity: memory.Entity{ID: id("invoicing"), Repo: ns, Name: "invoicing", Type: "Module", CreatedAt: "2026-10-25T08:26:19Z", UpdatedAt: "2026-10-25T08:26:19Z"},
			Score:  0.0333333, Via: "both",
		}},
		Graph: memory.Graph{
			Notice: memory.Notice,
			Entities: []memory.Entity{
				{ID: id("invoicing"), Repo: ns, Name: "invoicing", Type: "Module"},
				{ID: id("maria"), Repo: ns, Name: "maria", Type: "Person"},
			},
			Facts: []memory.Fact{
				{ID: "f1", Repo: ns, Subject: id("maria"), Predicate: "OWNS", Object: id("invoicing"), Source: "claude-code (hook)", Confidence: 1, ValidFrom: "2026-10-25T08:26:19Z"},
				{ID: "f2", Repo: ns, Subject: id("maria"), Predicate: "REVIEWS", Object: id("invoicing"), Confidence: 0.8, ValidTo: "2026-10-26T00:00:00Z"},
				{ID: "f3", Repo: ns, Subject: id("maria"), Predicate: "LIKES", Object: id("tea")}, // "tea" is not in Entities: strip the namespace
			},
		},
	}
	got := in.lean()
	assert.Equal(t, got.Facts, []leanFact{
		{Subject: "maria", Predicate: "OWNS", Object: "invoicing"},
		{Subject: "maria", Predicate: "REVIEWS", Object: "invoicing", Certainty: 0.8, Replaced: true},
		{Subject: "maria", Predicate: "LIKES", Object: "tea"},
	})
	assert.Equal(t, got.Entities, map[string]string{"invoicing": "Module", "maria": "Person"})
	assert.Equal(t, got.Matches, []leanMatch{{Name: "invoicing", Via: "both", Hit: 0.033}})

	before, err := json.Marshal(in)
	assert.NoError(t, err)
	after, err := json.Marshal(got)
	assert.NoError(t, err)
	assert.NotContains(t, string(after), ns, "no namespace in the answer")
	assert.True(t, len(after)*2 < len(before), "the lean answer is under half the stored form: %d vs %d bytes", len(after), len(before))
}
