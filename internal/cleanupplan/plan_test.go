package cleanupplan

import (
	"context"
	"strings"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/cleanup"
	"github.com/parisnakitakejser/codesynapse/internal/duplicates"
	"github.com/parisnakitakejser/codesynapse/internal/license"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/query"
	"github.com/parisnakitakejser/codesynapse/internal/security"
	"github.com/parisnakitakejser/codesynapse/internal/store"
	"github.com/parisnakitakejser/codesynapse/internal/store/memory"
	"github.com/parisnakitakejser/codesynapse/internal/store/storetest"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func item(name, loc, category, conf string, lines int, tags ...string) cleanup.Item {
	return cleanup.Item{Ref: query.Ref{ID: "r|Function|" + name, Name: name, Location: loc}, Category: category, Confidence: conf, LOC: lines, Security: tags, Reason: "x"}
}

func byKind(p Plan) map[string][]Step {
	m := map[string][]Step{}
	for _, s := range p.Steps {
		m[s.Kind] = append(m[s.Kind], s)
	}
	return m
}

func TestPrioritise(t *testing.T) {
	s := Step{Value: 80, Effort: 2, Safety: "medium"}
	prioritise(&s)
	assert.Equal(t, s.Priority, 42.7, "80 x 0.8 / 1.5")
	s = Step{Value: 50, Effort: 1, Safety: "high"}
	prioritise(&s)
	assert.Equal(t, s.Priority, 50.0)
	s = Step{Value: 90, Effort: 3, Safety: "low"}
	prioritise(&s)
	assert.Equal(t, s.Priority, 27.0)
}

func TestBuildRanksAndExplains(t *testing.T) {
	in := Inputs{
		Repo: "org/app",
		Cleanup: &cleanup.Report{
			Items: []cleanup.Item{
				item("old", "internal/legacy/a.go:10-60", "unreachable", "high", 50, "exec"),
				item("old2", "internal/legacy/b.go:1-30", "unreachable", "high", 30),
				item("old3", "internal/legacy/c.go:1-30", "unreachable", "high", 30),
				item("Exported", "pkg/x.go:1-20", "unreachable", "medium", 20),
				item("onlyTests", "pkg/y.go:1-20", "test-only", "high", 20),
			},
			Files:   []cleanup.FileItem{{File: "internal/dead.go", Symbols: 3, LOC: 200}, {File: "internal/testdead.go", Symbols: 1, LOC: 500, TestOnly: true}},
			Modules: []cleanup.ModuleItem{{Path: "github.com/old/lib", Version: "v1", Vulns: []string{"GO-1", "GO-2"}}},
		},
		Vulns: []security.Vulnerability{
			{ID: "GHSA-1", Severity: "high", Module: "golang.org/x/net", Version: "v0.17.0", Fixed: "v0.23.0", Reachability: "called"},
			{ID: "GHSA-2", Severity: "high", Module: "github.com/dep/dep", Version: "v1", Fixed: "v1.2", Reachability: "required", Via: "github.com/parent/p > github.com/mid/m"},
			{ID: "GHSA-3", Severity: "low", Module: "github.com/low/low", Version: "v1", Reachability: "required"},
			{ID: "GO-1", Severity: "high", Module: "github.com/old/lib", Version: "v1", Reachability: "required"},
		},
		Licences: &license.Report{Review: 2, Issues: []license.Entry{
			{Ecosystem: "Go", Name: "github.com/gpl/lib", Version: "v1", License: "GPL-3.0", Verdict: license.Verdict{Level: "conflict", Reason: "strong copyleft"}},
			{Ecosystem: "Go", Name: "github.com/gpl/devonly", Version: "v1", Dev: true, Verdict: license.Verdict{Level: "conflict"}},
		}},
		Duplicates: &duplicates.Report{Groups: []duplicates.Group{
			{Hash: "h1", Kind: "exact", Similarity: 1, Scope: "file", Copies: 3, LOC: 20, Redundant: 40, Members: []duplicates.Member{{ID: "a", Name: "pkg.Foo", File: "a.go", StartLine: 1, EndLine: 20}, {ID: "b", Name: "pkg.Bar", File: "a.go", StartLine: 30, EndLine: 50}}},
			{Hash: "h2", Kind: "near", Similarity: 0.8, Scope: "repository", Copies: 2, LOC: 40, Redundant: 35, Members: []duplicates.Member{{ID: "c", Name: "x.Do"}, {ID: "d", Name: "y.Do"}}},
		}},
		Hotspots: []Hotspot{
			{Hotspot: query.Hotspot{Ref: query.Ref{ID: "h", Name: "app.Big", Location: "app/big.go:1-90"}, Complexity: 30, FileChurn: 12, FanIn: 4, Score: 390}, Tested: false},
			{Hotspot: query.Hotspot{Ref: query.Ref{ID: "t", Name: "app.Tested"}, Complexity: 20, Score: 200}, Tested: true},
		},
	}
	p := Build(in)
	k := byKind(p)

	require.Len(t, k[RemoveRiskyDeadCode], 1)
	risky := k[RemoveRiskyDeadCode][0]
	assert.Contains(t, risky.Title, "exec")
	assert.Equal(t, risky.Safety, "high")
	require.Len(t, k[RemoveDeadCode], 1, "the two plain functions in one directory make one step")
	assert.Equal(t, k[RemoveDeadCode][0].Lines, 60)
	assert.Len(t, k[RemoveDeadFile], 1, "a test-only file is not a quick win")
	require.Len(t, k[RemoveDependency], 1)
	assert.Equal(t, k[RemoveDependency][0].Advisory, 2)

	up := map[string]Step{}
	for _, s := range k[UpgradeDependency] {
		up[s.Targets[0].Name] = s
	}
	require.Len(t, up, 2, "the unused module's advisory is solved by removing it; the low one is a note")
	called := up["golang.org/x/net@v0.17.0"]
	assert.Contains(t, called.Why, "calls the vulnerable code")
	assert.Equal(t, called.Effort, 2)
	indirect := up["github.com/dep/dep@v1"]
	assert.Contains(t, indirect.Title, "github.com/parent/p", "an indirect package is fixed by upgrading what pulls it in")
	assert.Equal(t, indirect.Effort, 3)

	require.Len(t, k[ResolveLicence], 1, "development-only conflicts are not planned")
	assert.Equal(t, k[ResolveLicence][0].Safety, "low")
	require.Len(t, k[MergeDuplicates], 1)
	assert.Equal(t, k[MergeDuplicates][0].Effort, 1, "same file")
	require.NotNil(t, k[MergeDuplicates][0].Group)
	assert.Equal(t, *k[MergeDuplicates][0].Group, GroupRef{Hash: "h1", Kind: "exact"}, "enough to ask refactor_duplicates how to merge it")
	assert.Contains(t, k[MergeDuplicates][0].Link, "/duplicates?group=h1&kind=exact")
	require.Len(t, k[ReviewEditedCopies], 1)
	require.Len(t, k[TestHotspot], 1, "a tested hotspot needs no tests")

	for i := 1; i < len(p.Steps); i++ {
		assert.True(t, p.Steps[i-1].Priority >= p.Steps[i].Priority, "highest priority first")
	}
	for _, s := range p.Steps {
		assert.True(t, s.Title != "" && s.Why != "" && s.Verify != "" && len(s.Targets) > 0, "every step says what, why and how to check: %s", s.ID)
		assert.True(t, s.Safety == "high" || s.Safety == "medium" || s.Safety == "low")
	}
	assert.Equal(t, p.Summary.Steps, len(p.Steps))
	assert.Equal(t, p.Summary.Advisories, 2+1+1, "removal fixes two, the upgrades one each")
	notes := strings.Join(p.Notes, "\n")
	assert.Contains(t, notes, "2 unused functions", "exported and test-only code is left to a person")
	assert.Contains(t, notes, "1 advisory")
	assert.Contains(t, notes, "2 dependencies have licences to read")
}

func TestBuildIsStableAndHandlesNothing(t *testing.T) {
	p := Build(Inputs{Repo: "r"})
	assert.Equal(t, len(p.Steps), 0)
	assert.True(t, p.Steps != nil && p.Notes != nil, "empty lists encode as [] not null")

	in := Inputs{Repo: "r", Cleanup: &cleanup.Report{Modules: []cleanup.ModuleItem{{Path: "a"}, {Path: "b"}}}}
	one, two := Build(in), Build(in)
	require.Len(t, one.Steps, 2)
	assert.Equal(t, one.Steps[0].ID, two.Steps[0].ID)
	assert.True(t, strings.HasPrefix(one.Steps[0].ID, "remove-dependency-"))
}

func TestAssemble(t *testing.T) {
	ctx := context.Background()
	g := storetest.Fixture()
	rn, ok := g.Node(storetest.ID(model.KindRepository, storetest.Repo))
	require.True(t, ok)
	cl, err := cleanup.Encode(cleanup.Report{Modules: []cleanup.ModuleItem{{Path: "github.com/old/lib", Version: "v1"}}})
	require.NoError(t, err)
	rn.Cleanup = cl
	s := memory.New()
	require.NoError(t, s.ReplaceRepo(ctx, storetest.Repo, g))

	p, err := Assemble(ctx, store.ForRepository(s, storetest.Repo), storetest.Repo)
	require.NoError(t, err)
	byK := byKind(p)
	require.Len(t, byK[RemoveDependency], 1)
	assert.True(t, len(byK[TestHotspot]) <= maxHotspotSteps)
	assert.Contains(t, strings.Join(p.Notes, "\n"), "indexed before some reports existed", "no duplicate report was stored")

	_, err = Assemble(ctx, store.ForRepository(s, storetest.Repo), "nope")
	assert.Error(t, err)
}

func TestShortName(t *testing.T) {
	assert.Equal(t, shortName("(*github.com/x/y/cmd/tool.Opts).Run"), "(*tool.Opts).Run")
	assert.Equal(t, shortName("github.com/x/y/internal/store.newID"), "store.newID")
	assert.Equal(t, shortName("app.Handler"), "app.Handler")
}

func TestUpgradeStepSaysWhenNoFixIsListed(t *testing.T) {
	p := Build(Inputs{Repo: "r", Vulns: []security.Vulnerability{
		{ID: "GHSA-9", Severity: "high", Module: "braces", Version: "3.0.3", Reachability: "required"},
		{ID: "GHSA-8", Severity: "high", Module: "forge", Version: "1.4.0", Fixed: "1.5.0", Reachability: "required"},
	}})
	byTitle := map[string]Step{}
	for _, s := range p.Steps {
		byTitle[s.Title] = s
	}
	none, ok := byTitle["Look into 1 advisory in braces (no fixed version listed yet)"]
	require.True(t, ok, "%v", byTitle)
	assert.Contains(t, none.Why, "no fixed version is listed yet")
	assert.False(t, strings.Contains(none.Why, "a fixed version"))
	fixed, ok := byTitle["Upgrade forge to fix 1 advisory"]
	require.True(t, ok)
	assert.Contains(t, fixed.Why, "the fix is in 1.5.0")
}
