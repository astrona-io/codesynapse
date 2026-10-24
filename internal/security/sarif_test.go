package security

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/astrona-io/codesynapse/internal/query"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestWriteSARIF(t *testing.T) {
	owner := &query.Ref{Name: "run"}
	rep := Report{
		Findings: []Finding{
			{Rule: "exec-variable", Title: "Process started from a non-constant program name", Severity: "medium", Location: "cmd/run.go:40", Detail: "second", Owner: owner},
			{Rule: "exec-variable", Title: "Process started from a non-constant program name", Severity: "medium", Location: "cmd/run.go:12-14", Detail: "first", Owner: owner, Exposed: true},
			{Rule: "not-in-catalog", Title: "Something", Severity: "info", Location: "x.go"},
			{Rule: "exec-variable", Severity: "medium", Location: ""}, // nowhere to put it
		},
		Suppressed: []Finding{{Rule: "exec-variable", Severity: "medium", Location: "tools/gen.go:3", SuppressionReason: "build tool, fixed input"}},
		Vulnerabilities: []Vulnerability{
			{ID: "GO-2026-0001", Summary: "Bad parse", Severity: "high", Module: "example.com/lib", Version: "v1.0.0", Fixed: "v1.0.1", Reachability: "called",
				URL: "https://pkg.go.dev/vuln/GO-2026-0001", CallSites: []query.Ref{{Location: "main.go:9"}}},
			{ID: "GHSA-aaaa", Summary: "Prototype pollution", Severity: "critical", Module: "left-pad", Ecosystem: "npm", Version: "1.0.0", Reachability: "imported"},
			{ID: "GHSA-bbbb", Severity: "low", Module: "requests", Ecosystem: "PyPI", Version: "2.0.0", Reachability: "required"},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, WriteSARIF(&buf, rep, SARIFOptions{Version: "v1.4.0", Manifest: func(eco string) string {
		return map[string]string{"npm": "web/package-lock.json"}[eco]
	}}))

	var log struct {
		Version string `json:"version"`
		Runs    []struct {
			Tool struct {
				Driver struct {
					Name  string `json:"name"`
					Rules []struct {
						ID         string         `json:"id"`
						HelpURI    string         `json:"helpUri"`
						Properties map[string]any `json:"properties"`
					} `json:"rules"`
				} `json:"driver"`
			} `json:"tool"`
			Results []struct {
				RuleID       string            `json:"ruleId"`
				RuleIndex    int               `json:"ruleIndex"`
				Level        string            `json:"level"`
				Fingerprints map[string]string `json:"partialFingerprints"`
				Suppressions []map[string]any  `json:"suppressions"`
				Locations    []struct {
					Physical struct {
						Artifact struct {
							URI string `json:"uri"`
						} `json:"artifactLocation"`
						Region *struct {
							StartLine int `json:"startLine"`
							EndLine   int `json:"endLine"`
						} `json:"region"`
					} `json:"physicalLocation"`
				} `json:"locations"`
			} `json:"results"`
			Properties map[string]any `json:"properties"`
		} `json:"runs"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &log))
	assert.Equal(t, log.Version, "2.1.0")
	require.Len(t, log.Runs, 1)
	run := log.Runs[0]
	assert.Equal(t, run.Tool.Driver.Name, "CodeSynapse")

	var placed []string
	prints := map[string]bool{}
	for _, r := range run.Results {
		require.Len(t, r.Locations, 1)
		assert.Equal(t, run.Tool.Driver.Rules[r.RuleIndex].ID, r.RuleID)
		placed = append(placed, r.RuleID+" "+r.Locations[0].Physical.Artifact.URI)
		prints[r.Fingerprints["codesynapse/v1"]] = true
	}
	assert.Equal(t, placed, []string{
		"exec-variable cmd/run.go", "exec-variable cmd/run.go", "exec-variable tools/gen.go", "not-in-catalog x.go",
		"GO-2026-0001 main.go", "GHSA-aaaa web/package-lock.json",
	}, "sorted by place; a vulnerability goes to its first call, else its manifest")
	assert.Len(t, prints, len(run.Results), "two findings of one rule in one function keep apart")
	assert.Equal(t, run.Properties["vulnerabilities_without_location"], any(float64(1)))

	first := run.Results[0]
	assert.Equal(t, first.Level, "warning")
	assert.Equal(t, first.Locations[0].Physical.Region.StartLine, 12)
	assert.Equal(t, first.Locations[0].Physical.Region.EndLine, 14)
	assert.Len(t, run.Results[2].Suppressions, 1)
	assert.Equal(t, run.Results[2].Suppressions[0]["justification"], any("build tool, fixed input"))
	assert.Equal(t, run.Results[5].Level, "error")

	rules := map[string]map[string]any{}
	for _, r := range run.Tool.Driver.Rules {
		rules[r.ID] = r.Properties
		if r.ID == "exec-variable" {
			assert.Equal(t, r.HelpURI, "https://github.com/astrona-io/codesynapse/blob/v1.4.0/rules/code/exec-variable.yaml")
		}
	}
	assert.Len(t, rules, 4)
	assert.Equal(t, rules["exec-variable"]["source"], any("CodeSynapse built-in rule"))
	assert.Equal(t, rules["GHSA-aaaa"]["source"], any("osv.dev advisory"))
	assert.Equal(t, rules["GHSA-aaaa"]["security-severity"], any("9.5"))
	_, ok := rules["not-in-catalog"]["security-severity"]
	assert.False(t, ok, "info has no security severity")
}
