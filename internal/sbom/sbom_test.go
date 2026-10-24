package sbom

import (
	"encoding/json"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestPURL(t *testing.T) {
	tests := []struct{ eco, name, version, want string }{
		{"Go", "github.com/spf13/cobra", "v1.8.0", "pkg:golang/github.com/spf13/cobra@v1.8.0"},
		{"Go", "stdlib", "v1.27.0", ""},
		{"npm", "left-pad", "1.3.0", "pkg:npm/left-pad@1.3.0"},
		{"npm", "@angular/core", "17.0.0", "pkg:npm/%40angular/core@17.0.0"},
		{"PyPI", "Django_Rest", "3.14.0", "pkg:pypi/django-rest@3.14.0"},
		{"crates.io", "serde", "1.0.190", "pkg:cargo/serde@1.0.190"},
		{"Packagist", "monolog/monolog", "3.5.0", "pkg:composer/monolog/monolog@3.5.0"},
		{"RubyGems", "rack", "3.0.8", "pkg:gem/rack@3.0.8"},
		{"NuGet", "Newtonsoft.Json", "13.0.1", "pkg:nuget/Newtonsoft.Json@13.0.1"},
		{"Maven", "com.google.guava:guava", "32.1.3-jre", "pkg:maven/com.google.guava/guava@32.1.3-jre"},
		{"Go", "example.com/x", "v1.0.0+incompatible", "pkg:golang/example.com/x@v1.0.0%2Bincompatible"},
		{"Conan", "zlib", "1.3", ""},
	}
	for _, tt := range tests {
		t.Run(tt.eco+" "+tt.name, func(t *testing.T) {
			assert.Equal(t, PURL(tt.eco, tt.name, tt.version), tt.want)
		})
	}
}

func fixtureGraph() (model.Node, store.DependencyGraph) {
	repo := model.Node{ID: "r|Repository|r", Kind: model.KindRepository, Repo: "r", Name: "r", Head: "abc123", Time: "2026-10-22T10:00:00Z", License: "Apache-2.0"}
	mod := func(name, version, eco string, indirect, dev bool, lic string) model.Node {
		return model.Node{ID: model.NodeID("r", model.KindModule, name), Kind: model.KindModule, Repo: "r", Name: name, Version: version,
			Ecosystem: eco, Indirect: indirect, Dev: dev, License: lic, Via: map[bool]string{true: "a", false: ""}[indirect]}
	}
	g := store.DependencyGraph{
		Modules: []model.Node{
			mod("github.com/a/a", "v1.0.0", "Go", false, false, "MIT"),
			mod("github.com/b/b", "v2.0.0", "Go", true, false, "GPL-3.0 OR MIT"),
			mod("@scope/tool", "1.0.0", "npm", false, true, "Weird Custom Terms"),
			mod("stdlib", "v1.27.0", "Go", false, false, ""),
		},
		Edges: []model.Edge{{From: "r|Module|github.com/a/a", To: "r|Module|github.com/b/b", Kind: model.EdgeDependsOn}},
		Vulnerabilities: []model.Node{{ID: "r|Vulnerability|b@GO-1", Kind: model.KindVulnerability, Name: "GO-1", Severity: "high", Package: "github.com/b/b",
			Ecosystem: "Go", Version: "v2.0.0", FixedVersion: "v2.1.0", Summary: "bad", URL: "https://pkg.go.dev/vuln/GO-1", Reachability: "imported"}},
	}
	return repo, g
}

func TestBuild(t *testing.T) {
	repo, g := fixtureGraph()
	doc := Build(repo, g, Options{Tool: "1.2.3"})
	assert.Equal(t, doc.BOMFormat, "CycloneDX")
	assert.Equal(t, doc.SpecVersion, "1.5")
	assert.Equal(t, doc.Metadata.Component.Name, "r")
	assert.Equal(t, doc.Metadata.Timestamp, "2026-10-22T10:00:00Z", "the ingest time, not the export time")
	assert.Equal(t, doc.Metadata.Tools.Components[0].Version, "1.2.3")
	require.Len(t, doc.Components, 4)

	by := map[string]Component{}
	for _, c := range doc.Components {
		by[c.Name] = c
	}
	a := by["github.com/a/a"]
	assert.Equal(t, a.PURL, "pkg:golang/github.com/a/a@v1.0.0")
	assert.Equal(t, a.BOMRef, a.PURL)
	assert.Equal(t, a.Scope, "required")
	assert.Equal(t, a.Licenses[0].License.ID, "MIT")
	b := by["github.com/b/b"]
	assert.Equal(t, b.Licenses[0].Expression, "GPL-3.0 OR MIT")
	tool := by["tool"]
	assert.Equal(t, tool.Group, "@scope")
	assert.Equal(t, tool.Scope, "excluded", "development dependencies are not shipped")
	assert.Equal(t, tool.Licenses[0].License.Name, "Weird Custom Terms", "unknown licences keep their name")
	std := by["Go standard library"]
	assert.Equal(t, std.Type, "platform")
	assert.Equal(t, std.PURL, "")

	// Dependencies: the project depends on the declared modules, a on b.
	deps := map[string][]string{}
	for _, d := range doc.Dependencies {
		deps[d.Ref] = d.DependsOn
	}
	assert.Equal(t, deps[a.BOMRef], []string{b.BOMRef})
	assert.Contains(t, deps["root"], a.BOMRef)
	assert.Contains(t, deps["root"], tool.BOMRef)
	for _, r := range deps["root"] {
		assert.True(t, r != b.BOMRef, "indirect modules hang under the module that requires them")
	}

	require.Len(t, doc.Vulnerabilities, 1)
	v := doc.Vulnerabilities[0]
	require.NotNil(t, v.Analysis)
	assert.Equal(t, v.Analysis.State, StateInTriage, "a module-level advisory proves nothing either way")
	assert.Equal(t, v.ID, "GO-1")
	assert.Equal(t, v.Affects[0].Ref, b.BOMRef)
	assert.Equal(t, v.Ratings[0].Severity, "high")
	assert.Equal(t, v.Recommendation, "Upgrade to v2.1.0 or later.")

	// Deterministic, valid JSON.
	one, err := Marshal(doc)
	require.NoError(t, err)
	two, err := Marshal(Build(repo, g, Options{Tool: "1.2.3"}))
	require.NoError(t, err)
	assert.Equal(t, string(one), string(two))
	var back map[string]any
	require.NoError(t, json.Unmarshal(one, &back))
	assert.Equal(t, back["bomFormat"], "CycloneDX")
	assert.Contains(t, back["serialNumber"], "urn:uuid:")

	g.Truncated = true
	trunc := Build(repo, g, Options{})
	found := false
	for _, p := range trunc.Metadata.Properties {
		found = found || p.Name == "codesynapse:truncated"
	}
	assert.True(t, found, "a cut list is said so in the document")
}

func TestAnalysisOf(t *testing.T) {
	for _, tt := range []struct {
		name          string
		node          model.Node
		state, reason string
	}{
		{"packages not in the build", model.Node{Reachability: "required", CodeAbsent: true}, StateNotAffected, JustificationNotPresent},
		{"called", model.Node{Reachability: "called"}, StateInTriage, ""},
		{"imported, no direct call found", model.Node{Reachability: "imported"}, StateInTriage, ""},
		{"only required", model.Node{Reachability: "required"}, StateInTriage, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := analysisOf(tt.node)
			assert.Equal(t, a.State, tt.state)
			assert.Equal(t, a.Justification, tt.reason)
			assert.NotEmpty(t, a.Detail)
		})
	}
}
