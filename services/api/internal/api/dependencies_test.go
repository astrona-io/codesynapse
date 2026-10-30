package api

import (
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestDependenciesOf(t *testing.T) {
	mod := func(eco, name, lic string, indirect, dev bool) model.Node {
		return model.Node{Kind: model.KindModule, Ecosystem: eco, Name: name, Version: "1.0.0", License: lic, Indirect: indirect, Dev: dev}
	}
	got := dependenciesOf(store.DependencyGraph{Modules: []model.Node{
		mod("", "stdlib", "BSD-3-Clause", false, false),
		mod("", "github.com/spf13/cobra", "Apache-2.0", false, false),
		mod("npm", "vue", "MIT", false, false),
		mod("npm", "vitest", "MIT", true, true),
		mod("PyPI", "requests", "", true, false),
	}})
	assert.Equal(t, got.ByEcosystem, []EcosystemCount{{"npm", 2}, {"Go", 1}, {"PyPI", 1}})
	assert.Len(t, got.Packages, 4, "Go's stdlib is left out, as in the licence report")
	assert.Equal(t, got.Packages[0], Package{Ecosystem: "Go", Name: "github.com/spf13/cobra", Version: "1.0.0", License: "Apache-2.0", Direct: true})
	assert.Equal(t, got.Packages[1].License, "unknown", "the licence report's name for no licence")
	assert.Equal(t, got.Packages[3], Package{Ecosystem: "npm", Name: "vue", Version: "1.0.0", License: "MIT", Direct: true})
}

func TestScorecardOf(t *testing.T) {
	assert.True(t, scorecardOf(model.Node{Scorecard: 5}) == nil, "no date, no Scorecard")
	got := scorecardOf(model.Node{Scorecard: 0, ScorecardDate: "2026-10-19", ScorecardChecks: []string{"Code-Review:0", "Made-Up:1", "Maintained:2"}})
	assert.Equal(t, *got, Scorecard{Score: 0, Date: "2026-10-19", Checks: []ScorecardCheck{{"Code-Review", 0}, {"Maintained", 2}}})
}
