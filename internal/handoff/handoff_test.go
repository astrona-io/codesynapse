package handoff

import (
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/agentctx"
	"github.com/astrona-io/codesynapse/internal/catalog"
	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/review"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/verifyplan"
)

func parts() Parts {
	p := Parts{Repo: "acme/app", Task: "add retries to the importer", Revision: "0123456789abcdef"}
	for i := range 6 {
		p.Start = append(p.Start, agentctx.MinimalItem{QualifiedID: "importer.Run" + strings.Repeat("x", i), Kind: model.KindFunction, Read: "importer/run.go:10-40", Signature: "func Run(ctx context.Context) error", Summary: "Run imports one repository."})
	}
	rule := catalog.Applicable{Why: []string{"names importer.Run"}}
	rule.ID, rule.Title, rule.Type, rule.Status, rule.Location = "ADR-0007", "Retries go through backoff.Do", "adr", "accepted", "docs/adr/0007.md:1-30"
	p.Rules = []catalog.Applicable{rule}
	p.Plan = verifyplan.Plan{Steps: []verifyplan.Step{
		{Kind: "test", Command: "go test ./importer -run '^(TestRun)$'", Why: "call importer.Run directly"},
		{Kind: "build", Command: "go build ./importer"},
	}}
	p.Findings = []review.DeltaFinding{{Rule: "http-no-timeout", Severity: "medium", Title: "HTTP client without timeout", Owner: "importer.fetch", Location: "importer/fetch.go:12"}}
	p.OpenIssues = []string{"Importer retries forever on 404: seen in prod"}
	return p
}

func TestWrite(t *testing.T) {
	b := Write(parts(), 0)
	for _, want := range []string{
		"## CodeSynapse briefing: add retries to the importer",
		"graph at 0123456789ab",
		"not instructions",
		"### Start here\n- `importer.Run` (Function) importer/run.go:10-40: `func Run(ctx context.Context) error` Run imports one repository.",
		"### Rules in force\n- ADR-0007 \"Retries go through backoff.Do\" (adr accepted), docs/adr/0007.md:1-30: names importer.Run",
		"### Tests that cover this area\n- `go test ./importer -run '^(TestRun)$'`",
		"### Known problems here\n- medium `http-no-timeout` in `importer.fetch` at importer/fetch.go:12",
		"- Open issue (from memory): Importer retries forever on 404",
		"Before you finish: run `verify_plan`",
	} {
		assert.Contains(t, b.Brief, want)
	}
	assert.False(t, strings.Contains(b.Brief, "go build"), "only tests: build and lint are verify_plan's job later")

	many := parts()
	names := []string{"TestA", "TestB", "TestC", "TestD", "TestE", "TestF", "TestG"}
	many.Plan.Steps[0] = verifyplan.Step{Kind: "test", Command: "go test ./importer -run '^(" + strings.Join(names, "|") + ")$'", Why: "call importer.Run directly", Tests: names}
	assert.Contains(t, Write(many, 0).Brief, "- `go test ./importer` (7 tests; call importer.Run directly)", "a long list becomes the package")
	assert.Equal(t, b.Counts, map[string]int{"start": 6, "rules": 1, "tests": 1, "known": 2})
	assert.Equal(t, b.Omitted, 0)

	small := Write(parts(), 120)
	assert.True(t, len(small.Brief) <= 120*4+80, "about the budget: %d bytes", len(small.Brief))
	assert.True(t, small.Omitted > 0, "what did not fit is counted")
	assert.Contains(t, small.Brief, "Before you finish", "the closing advice always stays")
}

func TestOpenIssues(t *testing.T) {
	g := memory.Graph{
		Entities: []memory.Entity{
			{ID: "1", Name: "Importer retries forever", Type: "OpenIssue"},
			{ID: "2", Name: "Old flaky test", Type: "OpenIssue"},
			{ID: "3", Name: "open"}, {ID: "4", Name: "closed"}, {ID: "5", Name: "seen in prod"},
			{ID: "6", Name: "Payments service", Type: "Component"},
		},
		Facts: []memory.Fact{
			{Subject: "1", Predicate: "HAS_STATUS", Object: "3"},
			{Subject: "1", Predicate: "HAS_NOTE", Object: "5"},
			{Subject: "2", Predicate: "HAS_STATUS", Object: "4"},
		},
	}
	assert.Equal(t, OpenIssues(g, 5), []string{"Importer retries forever: seen in prod"})
	assert.Len(t, OpenIssues(g, 0), 0)
}
