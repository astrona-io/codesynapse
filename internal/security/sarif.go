package security

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/astrona-io/codesynapse/rules"
)

// SARIF 2.1.0 (security roadmap item 7): the report in the format GitHub
// code scanning and IDEs read. Every result names where it comes from: a
// CodeSynapse built-in rule, with its rule file as help, or an osv.dev
// advisory. Code scanning needs a location for each result, so a
// vulnerability is placed at the first call of the vulnerable symbol, else
// at the manifest of its ecosystem; one with neither is left out and
// counted in the run's properties.

const (
	sarifSchema    = "https://json.schemastore.org/sarif-2.1.0.json"
	sarifInfoURI   = "https://codesynapse.astrona.io/"
	sarifSourceURI = "https://github.com/astrona-io/codesynapse/blob/"
)

// SARIFOptions are what the report does not carry.
type SARIFOptions struct {
	// Version is the CodeSynapse version that made the report.
	Version string
	// Manifest is the repository-relative manifest or lockfile of an
	// ecosystem ("Go", "npm", …), or "" when there is none.
	Manifest func(ecosystem string) string
}

type sarifLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool       sarifTool      `json:"tool"`
	Results    []sarifResult  `json:"results"`
	Properties map[string]any `json:"properties,omitempty"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name           string      `json:"name"`
	Version        string      `json:"version,omitempty"`
	InformationURI string      `json:"informationUri"`
	Rules          []sarifRule `json:"rules"`
}

type sarifText struct {
	Text     string `json:"text"`
	Markdown string `json:"markdown,omitempty"`
}

type sarifRule struct {
	ID                   string         `json:"id"`
	Name                 string         `json:"name,omitempty"`
	ShortDescription     sarifText      `json:"shortDescription"`
	FullDescription      *sarifText     `json:"fullDescription,omitempty"`
	Help                 *sarifText     `json:"help,omitempty"`
	HelpURI              string         `json:"helpUri,omitempty"`
	DefaultConfiguration map[string]any `json:"defaultConfiguration"`
	Properties           map[string]any `json:"properties"`
}

type sarifResult struct {
	RuleID              string            `json:"ruleId"`
	RuleIndex           int               `json:"ruleIndex"`
	Level               string            `json:"level"`
	Message             sarifText         `json:"message"`
	Locations           []sarifLocation   `json:"locations"`
	PartialFingerprints map[string]string `json:"partialFingerprints"`
	Suppressions        []sarifSuppress   `json:"suppressions,omitempty"`
	Properties          map[string]any    `json:"properties,omitempty"`
}

type sarifLocation struct {
	PhysicalLocation sarifPhysical `json:"physicalLocation"`
}

type sarifPhysical struct {
	ArtifactLocation sarifArtifact `json:"artifactLocation"`
	Region           *sarifRegion  `json:"region,omitempty"`
}

type sarifArtifact struct {
	URI       string `json:"uri"`
	URIBaseID string `json:"uriBaseId"`
}

type sarifRegion struct {
	StartLine int `json:"startLine"`
	EndLine   int `json:"endLine,omitempty"`
}

type sarifSuppress struct {
	Kind          string `json:"kind"`
	Justification string `json:"justification,omitempty"`
}

// WriteSARIF writes rep as a SARIF 2.1.0 log with one run.
func WriteSARIF(w io.Writer, rep Report, o SARIFOptions) error {
	run := sarifRun{
		Tool:    sarifTool{Driver: sarifDriver{Name: "CodeSynapse", Version: o.Version, InformationURI: sarifInfoURI, Rules: []sarifRule{}}},
		Results: []sarifResult{},
	}
	ruleIndex := map[string]int{}
	rule := func(r sarifRule) int {
		if i, ok := ruleIndex[r.ID]; ok {
			return i
		}
		ruleIndex[r.ID] = len(run.Tool.Driver.Rules)
		run.Tool.Driver.Rules = append(run.Tool.Driver.Rules, r)
		return ruleIndex[r.ID]
	}
	seen := map[string]int{}
	fingerprint := func(parts ...string) string {
		key := strings.Join(parts, "\x00")
		seen[key]++
		// Findings of one rule in one function and file are told apart by
		// their order, which survives edits elsewhere in the file.
		sum := sha256.Sum256([]byte(key + "\x00" + strconv.Itoa(seen[key])))
		return hex.EncodeToString(sum[:16])
	}

	findings := slices.Concat(rep.Findings, rep.Suppressed)
	slices.SortStableFunc(findings, func(a, b Finding) int { return compareLocation(a.Location, b.Location) })
	for _, f := range findings {
		file, start, end := splitLocation(f.Location)
		if file == "" {
			continue
		}
		owner := ""
		if f.Owner != nil {
			owner = f.Owner.Name
		}
		res := sarifResult{
			RuleID:    f.Rule,
			RuleIndex: rule(findingRule(f, o.Version)),
			Level:     sarifLevel(f.Severity),
			Message:   sarifText{Text: strings.TrimSpace(f.Title + ": " + f.Detail)},
			Locations: []sarifLocation{location(file, start, end)},
			PartialFingerprints: map[string]string{
				"codesynapse/v1": fingerprint(f.Rule, owner, file),
			},
			Properties: map[string]any{"exposed": f.Exposed, "tested": f.Tested, "priority": f.Priority},
		}
		if len(f.CodeOwners) > 0 {
			res.Properties["code_owners"] = f.CodeOwners
		}
		if f.SuppressionReason != "" {
			res.Suppressions = []sarifSuppress{{Kind: "external", Justification: f.SuppressionReason}}
		}
		run.Results = append(run.Results, res)
	}

	unplaced := 0
	for _, v := range rep.Vulnerabilities {
		loc, ok := vulnLocation(v, o.Manifest)
		if !ok {
			unplaced++
			continue
		}
		msg := fmt.Sprintf("%s %s is affected by %s: %s", v.Module, v.Version, v.ID, v.Summary)
		if v.Fixed != "" {
			msg += fmt.Sprintf(" Fixed in %s.", v.Fixed)
		}
		msg += " Reachability: " + v.Reachability + "."
		if v.Via != "" {
			msg += " Brought in via " + v.Via + "."
		}
		run.Results = append(run.Results, sarifResult{
			RuleID:              v.ID,
			RuleIndex:           rule(vulnRule(v)),
			Level:               sarifLevel(v.Severity),
			Message:             sarifText{Text: msg},
			Locations:           []sarifLocation{loc},
			PartialFingerprints: map[string]string{"codesynapse/v1": fingerprint(v.ID, v.Module, v.Version)},
			Properties:          map[string]any{"reachability": v.Reachability, "priority": v.Priority},
		})
	}
	if unplaced > 0 {
		run.Properties = map[string]any{"vulnerabilities_without_location": unplaced}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(sarifLog{Schema: sarifSchema, Version: "2.1.0", Runs: []sarifRun{run}})
}

func findingRule(f Finding, version string) sarifRule {
	r := sarifRule{
		ID: f.Rule, Name: ruleName(f.Rule), ShortDescription: sarifText{Text: f.Title},
		DefaultConfiguration: map[string]any{"level": sarifLevel(f.Severity)},
		Properties:           ruleProperties(f.Severity, f.CWE, "CodeSynapse built-in rule"),
	}
	if def, ok := rules.Get(f.Rule); ok {
		r.ShortDescription = sarifText{Text: def.Title}
		r.FullDescription = &sarifText{Text: def.Why}
		r.Help = &sarifText{Text: def.Why + "\n\nFix: " + def.Fix, Markdown: "**Why:** " + def.Why + "\n\n**Fix:** " + def.Fix}
		// The rule file of the release that ran, so the text matches.
		ref := "main"
		if semver.IsValid(version) {
			ref = version
		}
		r.HelpURI = sarifSourceURI + ref + "/" + def.File
		r.DefaultConfiguration["level"] = sarifLevel(def.Severity)
		r.Properties = ruleProperties(def.Severity, def.CWE, "CodeSynapse built-in rule")
	}
	return r
}

func vulnRule(v Vulnerability) sarifRule {
	return sarifRule{
		ID: v.ID, ShortDescription: sarifText{Text: cmp.Or(v.Summary, v.ID)}, HelpURI: v.URL,
		DefaultConfiguration: map[string]any{"level": sarifLevel(v.Severity)},
		Properties:           ruleProperties(v.Severity, v.CWE, "osv.dev advisory"),
	}
}

// ruleProperties carry the tags and the numeric severity code scanning
// sorts by, and where the rule comes from.
func ruleProperties(severity string, cwe []string, source string) map[string]any {
	tags := []string{"security"}
	for _, c := range cwe {
		tags = append(tags, "external/cwe/"+strings.ToLower(c))
	}
	p := map[string]any{"tags": tags, "source": source}
	if s, ok := securitySeverity[severity]; ok {
		p["security-severity"] = s
	}
	return p
}

// securitySeverity is GitHub's scale: 9.0+ critical, 7.0+ high, 4.0+
// medium, above 0 low.
var securitySeverity = map[string]string{"critical": "9.5", "high": "8.0", "medium": "5.5", "low": "3.0"}

func sarifLevel(severity string) string {
	switch severity {
	case "critical", "high":
		return "error"
	case "medium":
		return "warning"
	}
	return "note"
}

// ruleName is a rule ID in PascalCase, as SARIF names rules.
func ruleName(id string) string {
	var b strings.Builder
	for _, part := range strings.FieldsFunc(id, func(r rune) bool { return r == '-' || r == '_' || r == '.' }) {
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return b.String()
}

func vulnLocation(v Vulnerability, manifest func(string) string) (sarifLocation, bool) {
	for _, c := range v.CallSites {
		if file, start, end := splitLocation(c.Location); file != "" {
			return location(file, start, end), true
		}
	}
	if manifest == nil {
		return sarifLocation{}, false
	}
	if m := manifest(cmp.Or(v.Ecosystem, "Go")); m != "" {
		return location(m, 0, 0), true
	}
	return sarifLocation{}, false
}

func location(file string, start, end int) sarifLocation {
	l := sarifLocation{PhysicalLocation: sarifPhysical{ArtifactLocation: sarifArtifact{URI: file, URIBaseID: "%SRCROOT%"}}}
	if start > 0 {
		l.PhysicalLocation.Region = &sarifRegion{StartLine: start}
		if end > start {
			l.PhysicalLocation.Region.EndLine = end
		}
	}
	return l
}

// splitLocation reads "file", "file:12" or "file:12-20".
func splitLocation(loc string) (file string, start, end int) {
	i := strings.LastIndex(loc, ":")
	if i < 0 {
		return loc, 0, 0
	}
	lines := loc[i+1:]
	a, b, ranged := strings.Cut(lines, "-")
	start, err := strconv.Atoi(a)
	if err != nil {
		return loc, 0, 0
	}
	if ranged {
		end, _ = strconv.Atoi(b)
	}
	return loc[:i], start, end
}

func compareLocation(a, b string) int {
	fa, la, _ := splitLocation(a)
	fb, lb, _ := splitLocation(b)
	if c := strings.Compare(fa, fb); c != 0 {
		return c
	}
	return la - lb
}
