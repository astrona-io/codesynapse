// Package sbom writes a software bill of materials in the CycloneDX 1.5 JSON
// format from the dependency graph CodeSynapse already holds: the modules of
// a repository with their licences, which module requires which, and the
// vulnerabilities found in them.
//
// It lists what the lockfiles and go.mod resolved, not what a build finally
// ships: a bundler may leave packages out and a container image adds its own.
// Say so wherever the file is offered. Dependency edges are only present for
// the ecosystems whose lockfiles record them, and a missing edge list is
// recorded as a property of the document, never as "no dependencies".
package sbom

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/parisnakitakejser/codesynapse/internal/license"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/store"
)

// Document is a CycloneDX 1.5 BOM (the parts CodeSynapse fills in).
type Document struct {
	BOMFormat       string          `json:"bomFormat"`
	SpecVersion     string          `json:"specVersion"`
	SerialNumber    string          `json:"serialNumber"`
	Version         int             `json:"version"`
	Metadata        Metadata        `json:"metadata"`
	Components      []Component     `json:"components"`
	Dependencies    []Dependency    `json:"dependencies,omitempty"`
	Vulnerabilities []Vulnerability `json:"vulnerabilities,omitempty"`
}

// Metadata says what the document describes and how it was made.
type Metadata struct {
	Timestamp  string      `json:"timestamp,omitempty"`
	Tools      Tools       `json:"tools"`
	Component  Component   `json:"component"`
	Properties []Property  `json:"properties,omitempty"`
	Licenses   []LicenseID `json:"licenses,omitempty"`
}

// Tools lists the tools that made the document.
type Tools struct {
	Components []Component `json:"components"`
}

// Component is a library, platform or application.
type Component struct {
	Type       string      `json:"type"`
	BOMRef     string      `json:"bom-ref,omitempty"`
	Group      string      `json:"group,omitempty"`
	Name       string      `json:"name"`
	Version    string      `json:"version,omitempty"`
	Scope      string      `json:"scope,omitempty"`
	PURL       string      `json:"purl,omitempty"`
	Licenses   []LicenseID `json:"licenses,omitempty"`
	Properties []Property  `json:"properties,omitempty"`
}

// LicenseID is either an SPDX expression, a licence identifier or a name.
type LicenseID struct {
	Expression string   `json:"expression,omitempty"`
	License    *License `json:"license,omitempty"`
}

// License is a licence by identifier or by name.
type License struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// Property is a name and value CycloneDX has no field for.
type Property struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Dependency says which components one component depends on.
type Dependency struct {
	Ref       string   `json:"ref"`
	DependsOn []string `json:"dependsOn"`
}

// Vulnerability is a known vulnerability and the components it affects.
type Vulnerability struct {
	BOMRef         string     `json:"bom-ref"`
	ID             string     `json:"id"`
	Source         Source     `json:"source"`
	Ratings        []Rating   `json:"ratings,omitempty"`
	Description    string     `json:"description,omitempty"`
	Recommendation string     `json:"recommendation,omitempty"`
	Affects        []Affects  `json:"affects"`
	Properties     []Property `json:"properties,omitempty"`
}

// Source is where a vulnerability was reported.
type Source struct {
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

// Rating is a severity.
type Rating struct {
	Severity string `json:"severity"`
	Method   string `json:"method"`
}

// Affects points at an affected component.
type Affects struct {
	Ref string `json:"ref"`
}

// Options say where the document came from.
type Options struct {
	// Tool is the CodeSynapse version.
	Tool string
}

const rootRef = "root"

// Build turns the dependency graph of repo (its Repository node and the
// store's DependencyGraph) into a CycloneDX document. The output depends only
// on its inputs: the same ingest gives the same bytes.
func Build(repo model.Node, g store.DependencyGraph, opts Options) Document {
	doc := Document{BOMFormat: "CycloneDX", SpecVersion: "1.5", Version: 1, Components: []Component{}}
	sum := sha256.Sum256([]byte(repo.Repo + "|" + repo.Head + "|" + repo.Time))
	h := hex.EncodeToString(sum[:16])
	doc.SerialNumber = fmt.Sprintf("urn:uuid:%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
	doc.Metadata = Metadata{
		Timestamp: repo.Time,
		Tools:     Tools{Components: []Component{{Type: "application", Name: "codesynapse", Version: opts.Tool}}},
		Component: Component{Type: "application", BOMRef: rootRef, Name: cmp.Or(repo.Name, repo.Repo), Version: repo.Head},
	}
	if repo.License != "" {
		doc.Metadata.Licenses = licenseChoice(repo.License)
	}

	refOf := map[string]string{} // module node ID -> bom-ref
	byKey := map[string]string{} // ecosystem|name -> bom-ref, for vulnerabilities
	for _, m := range g.Modules {
		c := component(m)
		refOf[m.ID] = c.BOMRef
		byKey[moduleKey(m.Ecosystem, m.Name)] = c.BOMRef
		doc.Components = append(doc.Components, c)
	}
	slices.SortFunc(doc.Components, func(a, b Component) int { return cmp.Compare(a.BOMRef, b.BOMRef) })

	deps := map[string][]string{}
	for _, e := range g.Edges {
		from, to := refOf[e.From], refOf[e.To]
		if from != "" && to != "" && from != to {
			deps[from] = append(deps[from], to)
		}
	}
	for _, m := range g.Modules {
		if !m.Indirect {
			deps[rootRef] = append(deps[rootRef], refOf[m.ID])
		}
	}
	refs := make([]string, 0, len(deps))
	for r := range deps {
		refs = append(refs, r)
	}
	slices.Sort(refs)
	for _, r := range refs {
		doc.Dependencies = append(doc.Dependencies, Dependency{Ref: r, DependsOn: sortedUnique(deps[r])})
	}
	doc.Metadata.Properties = []Property{
		{Name: "codesynapse:scope", Value: "resolved by lockfiles and go.mod; not the contents of a built artifact"},
		{Name: "codesynapse:dependency-edges", Value: "present where the lockfile records them (Go, npm package-lock, uv, poetry, Cargo, Composer, Bundler, NuGet); absent for yarn, pnpm, Gradle and requirements files"},
	}
	if g.Truncated {
		doc.Metadata.Properties = append(doc.Metadata.Properties, Property{Name: "codesynapse:truncated", Value: "true: a size limit cut the component, vulnerability or dependency list"})
	}

	for _, v := range g.Vulnerabilities {
		ref := byKey[moduleKey(v.Ecosystem, v.Package)]
		if ref == "" {
			continue
		}
		vu := Vulnerability{
			BOMRef: "vuln:" + v.Name + ":" + ref, ID: v.Name, Source: Source{Name: "OSV", URL: v.URL},
			Description: v.Summary, Affects: []Affects{{Ref: ref}},
			Properties: []Property{{Name: "codesynapse:reachability", Value: v.Reachability}},
		}
		if v.Severity != "" {
			vu.Ratings = []Rating{{Severity: v.Severity, Method: "other"}}
		}
		if v.FixedVersion != "" {
			vu.Recommendation = "Upgrade to " + v.FixedVersion + " or later."
		}
		doc.Vulnerabilities = append(doc.Vulnerabilities, vu)
	}
	slices.SortFunc(doc.Vulnerabilities, func(a, b Vulnerability) int { return cmp.Compare(a.BOMRef, b.BOMRef) })
	return doc
}

// Marshal renders the document as indented JSON.
func Marshal(d Document) ([]byte, error) {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding SBOM: %w", err)
	}
	return append(b, '\n'), nil
}

func moduleKey(ecosystem, name string) string {
	if ecosystem == "" {
		ecosystem = "Go"
	}
	return ecosystem + "|" + name
}

func component(m model.Node) Component {
	eco := cmp.Or(m.Ecosystem, "Go")
	group, name := split(eco, m.Name)
	c := Component{Type: "library", Group: group, Name: name, Version: m.Version, Scope: "required",
		PURL: PURL(eco, m.Name, m.Version)}
	c.BOMRef = cmp.Or(c.PURL, eco+":"+m.Name+"@"+m.Version)
	if eco == "Go" && m.Name == "stdlib" {
		c.Type, c.Group, c.Name = "platform", "", "Go standard library"
	}
	if m.Dev {
		c.Scope = "excluded"
	}
	if m.License != "" {
		c.Licenses = licenseChoice(m.License)
	}
	c.Properties = []Property{
		{Name: "codesynapse:ecosystem", Value: eco},
		{Name: "codesynapse:direct", Value: fmt.Sprint(!m.Indirect)},
	}
	if m.Via != "" {
		c.Properties = append(c.Properties, Property{Name: "codesynapse:via", Value: m.Via})
	}
	if m.LicenseSource != "" {
		c.Properties = append(c.Properties, Property{Name: "codesynapse:license-source", Value: m.LicenseSource})
	}
	return c
}

// split gives CycloneDX's group and name for an ecosystem's package name.
func split(eco, name string) (group, n string) {
	switch eco {
	case "npm":
		if g, rest, ok := strings.Cut(name, "/"); ok && strings.HasPrefix(g, "@") {
			return g, rest
		}
	case "Maven":
		if g, a, ok := strings.Cut(name, ":"); ok {
			return g, a
		}
	case "Packagist":
		if g, a, ok := strings.Cut(name, "/"); ok {
			return g, a
		}
	}
	return "", name
}

// PURL is the package URL of a dependency (https://github.com/package-url/purl-spec),
// or "" for an ecosystem without one.
func PURL(eco, name, version string) string {
	seg := func(s string) string {
		return strings.NewReplacer("+", "%2B", "@", "%40").Replace(url.PathEscape(s))
	}
	path := func(parts ...string) string {
		out := make([]string, len(parts))
		for i, p := range parts {
			out[i] = seg(p)
		}
		return strings.Join(out, "/")
	}
	var t, p string
	switch eco {
	case "Go":
		if name == "stdlib" {
			return ""
		}
		t, p = "golang", path(strings.Split(name, "/")...)
	case "npm":
		t, p = "npm", path(strings.Split(name, "/")...)
	case "PyPI":
		t, p = "pypi", seg(strings.ReplaceAll(strings.ToLower(name), "_", "-"))
	case "crates.io":
		t, p = "cargo", seg(name)
	case "Packagist":
		t, p = "composer", path(strings.Split(name, "/")...)
	case "RubyGems":
		t, p = "gem", seg(name)
	case "NuGet":
		t, p = "nuget", seg(name)
	case "Maven":
		t, p = "maven", path(strings.Split(name, ":")...)
	default:
		return ""
	}
	s := "pkg:" + t + "/" + p
	if version != "" {
		s += "@" + seg(version)
	}
	return s
}

// licenseChoice maps a licence string to CycloneDX: an SPDX expression when it
// has operators, an identifier when it is a licence CodeSynapse knows, else a name.
func licenseChoice(s string) []LicenseID {
	s = strings.TrimSpace(s)
	switch {
	case strings.ContainsAny(s, "()") || strings.Contains(s, " OR ") || strings.Contains(s, " AND ") || strings.Contains(s, " WITH "):
		return []LicenseID{{Expression: s}}
	case license.Of(s) != license.Unknown:
		return []LicenseID{{License: &License{ID: license.Normalize(s)}}}
	}
	return []LicenseID{{License: &License{Name: s}}}
}

func sortedUnique(in []string) []string {
	slices.Sort(in)
	return slices.Compact(in)
}
