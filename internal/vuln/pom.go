package vuln

import (
	"encoding/xml"
	"regexp"
	"strings"
)

// Maven: pom.xml. It names the dependencies the project declares itself, with
// a version written out, taken from a <properties> entry or inherited from this
// file's own <dependencyManagement>. A version that comes from a parent POM or
// an imported BOM is in another file (often on a repository server), so that
// dependency is left out rather than guessed: OSV needs an exact version.

type pomDependency struct {
	GroupID    string `xml:"groupId"`
	ArtifactID string `xml:"artifactId"`
	Version    string `xml:"version"`
	Scope      string `xml:"scope"`
}

type pomProperty struct {
	XMLName xml.Name
	Value   string `xml:",chardata"`
}

type pomFile struct {
	GroupID string `xml:"groupId"`
	Version string `xml:"version"`
	Parent  struct {
		Version string `xml:"version"`
	} `xml:"parent"`
	Properties struct {
		All []pomProperty `xml:",any"`
	} `xml:"properties"`
	Managed      []pomDependency `xml:"dependencyManagement>dependencies>dependency"`
	Dependencies []pomDependency `xml:"dependencies>dependency"`
}

var pomPlaceholder = regexp.MustCompile(`\$\{([^}]+)\}`)

// parsePom reads the direct dependencies of one pom.xml.
func parsePom(data []byte, _ string, _ ...manifestScope) []Module {
	var p pomFile
	// encoding/xml expands no custom entities, and the file is size-capped.
	if err := xml.Unmarshal(data, &p); err != nil {
		return nil
	}
	props := map[string]string{}
	for _, e := range p.Properties.All {
		props[e.XMLName.Local] = strings.TrimSpace(e.Value)
	}
	version := strings.TrimSpace(p.Version)
	if version == "" {
		version = strings.TrimSpace(p.Parent.Version)
	}
	props["project.version"], props["pom.version"] = version, version
	resolve := func(v string) string {
		v = strings.TrimSpace(v)
		for range 4 { // a property may name another one
			next := pomPlaceholder.ReplaceAllStringFunc(v, func(m string) string {
				if val, ok := props[m[2:len(m)-1]]; ok {
					return val
				}
				return m
			})
			if next == v {
				break
			}
			v = next
		}
		if v == "" || strings.Contains(v, "${") || strings.ContainsAny(v, "[(,") {
			return "" // unresolved, or a version range
		}
		return v
	}
	managed := map[string]string{}
	for _, d := range p.Managed {
		if v := resolve(d.Version); v != "" {
			managed[strings.TrimSpace(d.GroupID)+":"+strings.TrimSpace(d.ArtifactID)] = v
		}
	}
	var out []Module
	for _, d := range p.Dependencies {
		name := strings.TrimSpace(d.GroupID) + ":" + strings.TrimSpace(d.ArtifactID)
		if strings.Contains(name, "${") || name == ":" {
			continue
		}
		v := resolve(d.Version)
		if v == "" {
			v = managed[name]
		}
		if v == "" {
			continue
		}
		out = append(out, Module{Path: name, Version: v, Ecosystem: EcosystemMaven, Dev: strings.EqualFold(strings.TrimSpace(d.Scope), "test")})
	}
	return out
}
