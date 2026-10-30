package nodeprops

import (
	"encoding/json"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestPropsRoundTrip(t *testing.T) {
	n := model.Node{
		ID: "r|Function|f", Kind: model.KindFunction, Repo: "r", Name: "f", QualifiedName: "p.f",
		SymbolID: "sym-0123456789abcdef0123456789abcdef", Revision: "abc", Identities: `{"version":1}`,
		File: "a.go", StartLine: 3, EndLine: 9, Complexity: 4, Exported: true, SecurityTags: []string{"exec", "sql"}, CloneHash: "0123456789abcdef", CloneSig: "00ff", DocType: "adr", Status: "accepted", Meta: `{"owner":"x"}`, Analysis: `{"version":1}`,
	}
	p := To(n)
	_, hasSummary := p["summary"]
	assert.False(t, hasSummary, "zero values are omitted")

	// JSON returns lists as []any.
	p["security_tags"] = []any{"exec", "sql"}
	assert.Equal(t, From(p), n)

	mod := model.Node{ID: "r|Module|npm:x", Kind: model.KindModule, Repo: "r", Name: "x", Version: "1.0.0", Ecosystem: "npm",
		License: "MIT OR Apache-2.0", LicenseSource: "package-lock.json", Dev: true, Indirect: true, Via: "a > b", Roots: 2,
		Latest: "2.0.0", Deprecated: "use y", Behind: 3, Retracted: "broken build",
		Scorecard: 3.7, ScorecardDate: "2026-10-19", ScorecardChecks: []string{"Code-Review:0", "Maintained:2"}}
	assert.Equal(t, From(To(mod)), mod)
	zero := model.Node{ID: "r|Module|z", Kind: model.KindModule, Repo: "r", Name: "z", ScorecardDate: "2026-10-19"}
	_, scored := To(zero)["scorecard"]
	assert.True(t, scored, "a score of 0 is kept beside its date")
	assert.Equal(t, From(To(zero)), zero)
	_, scored = To(model.Node{ID: "r|Module|y", Kind: model.KindModule})["scorecard"]
	assert.False(t, scored, "no date, no score")
	vuln := model.Node{ID: "r|Vulnerability|x@GO-1", Kind: model.KindVulnerability, Repo: "r", Name: "GO-1", Reachability: "required", CodeAbsent: true}
	assert.Equal(t, From(To(vuln)), vuln)
	finding := model.Node{ID: "r|Finding|f", Kind: model.KindFinding, Repo: "r", Name: "f", Rule: "exec-variable", Severity: "high", Ignore: "fixed input"}
	assert.Equal(t, From(To(finding)), finding)
	repoNode := model.Node{ID: "r|Repository|r", Kind: model.KindRepository, Repo: "r", Name: "r", License: "Apache-2.0", LicenseSource: "LICENSE", Licenses: `{"total":1}`, Coverage: `{"yaml":3}`, Duplicates: `{"groups":[]}`}
	assert.Equal(t, From(To(repoNode)), repoNode)
	assert.Equal(t, From(To(mod)), mod)
}

func TestIntForms(t *testing.T) {
	for _, v := range []any{int64(7), 7, float64(7), json.Number("7")} {
		assert.Equal(t, Int(v), 7)
	}
	assert.Equal(t, Int("7"), 0)
}

func TestFloatForms(t *testing.T) {
	for _, v := range []any{3.5, float32(3.5), json.Number("3.5")} {
		assert.Equal(t, Float(v), 3.5)
	}
	assert.Equal(t, Float(int64(3)), 3.0)
	assert.Equal(t, Float("3.5"), 0.0)
}
