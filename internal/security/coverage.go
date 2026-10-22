package security

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
)

// Coverage records what the security rules could read in one ingest, so a
// grade is always shown next to what was and was not checked.
type Coverage struct {
	// Code counts source files per language the code rules ran on.
	Code map[string]int `json:"code,omitempty"`
	// YAML counts parsed YAML files; YAMLUnparseable those the parser rejected.
	YAML            int `json:"yaml,omitempty"`
	YAMLUnparseable int `json:"yaml_unparseable,omitempty"`
	// Kubernetes counts manifests (resources) the Kubernetes rules ran on.
	Kubernetes int `json:"kubernetes,omitempty"`
	Workflows  int `json:"workflows,omitempty"`
	Compose    int `json:"compose,omitempty"`
	// HelmScanned of HelmTotal chart templates rendered to literal YAML the
	// rules could read.
	HelmScanned int `json:"helm_scanned,omitempty"`
	HelmTotal   int `json:"helm_total,omitempty"`
	// Dependencies counts modules and packages looked up for advisories.
	Dependencies int `json:"dependencies,omitempty"`
	// NotCovered counts files of kinds no rule reads yet, by kind.
	NotCovered map[string]int `json:"not_covered,omitempty"`
}

// k8sAPIVersion tells a Kubernetes resource entry (signature is its
// apiVersion) from a plain key-path entry.
var k8sAPIVersion = regexp.MustCompile(`^([a-z0-9.-]+/)?v\d`)

// CoverageOf counts what the rules read in g. notCovered comes from Inventory.
func CoverageOf(g *model.Graph, notCovered map[string]int) Coverage {
	c := Coverage{Code: map[string]int{}, NotCovered: map[string]int{}}
	k8sFiles := map[string]bool{}
	for _, n := range g.Nodes() {
		switch n.Kind {
		case model.KindFile:
			lang := "Go"
			if n.Language != "" {
				lang = n.Language
			}
			c.Code[lang]++
		case model.KindConfig:
			if strings.HasPrefix(n.Summary, "unparseable YAML") {
				c.YAMLUnparseable++
			} else {
				c.YAML++
			}
			if strings.HasPrefix(n.Summary, "Kubernetes:") {
				k8sFiles[n.File] = true
			}
			if isWorkflowFile(n.File) {
				c.Workflows++
			}
			if isComposeFile(n.File) {
				c.Compose++
			}
		case model.KindTemplate:
			if n.Package == "" {
				continue
			}
			c.HelmTotal++
			if n.Language == "yaml" {
				c.HelmScanned++
			}
		case model.KindModule:
			c.Dependencies++
		}
	}
	for _, n := range g.Nodes() {
		if n.Kind == model.KindConfigEntry && k8sFiles[n.File] && strings.Contains(n.Name, "/") && k8sAPIVersion.MatchString(n.Signature) {
			c.Kubernetes++
		}
	}
	for k, v := range notCovered {
		c.NotCovered[k] = v
	}
	return c
}

func isWorkflowFile(rel string) bool {
	return strings.HasPrefix(rel, ".github/workflows/") || strings.Contains(rel, "/.github/workflows/")
}

func isComposeFile(rel string) bool {
	b := filepath.Base(rel)
	return strings.HasPrefix(b, "docker-compose") || strings.HasPrefix(b, "compose.")
}

// Scannable reports whether any rule had something to read.
func (c Coverage) Scannable() bool {
	code := 0
	for _, n := range c.Code {
		code += n
	}
	return code > 0 || c.YAML > 0 || c.HelmScanned > 0 || c.Dependencies > 0
}

// Lines words the coverage as what was checked and what was not.
func (c Coverage) Lines() (checked, notChecked []string) {
	for _, lang := range sortedKeys(c.Code) {
		checked = append(checked, fmt.Sprintf("%d %s file(s)", c.Code[lang], lang))
	}
	if c.YAML > 0 {
		checked = append(checked, fmt.Sprintf("%d YAML file(s)", c.YAML))
	}
	if c.Kubernetes > 0 {
		checked = append(checked, fmt.Sprintf("%d Kubernetes resource(s)", c.Kubernetes))
	}
	if c.Workflows > 0 {
		checked = append(checked, fmt.Sprintf("%d GitHub Actions workflow(s)", c.Workflows))
	}
	if c.Compose > 0 {
		checked = append(checked, fmt.Sprintf("%d Compose file(s)", c.Compose))
	}
	if c.HelmTotal > 0 {
		checked = append(checked, fmt.Sprintf("%d of %d Helm template(s)", c.HelmScanned, c.HelmTotal))
	}
	if c.Dependencies > 0 {
		checked = append(checked, fmt.Sprintf("%d dependencies against known advisories", c.Dependencies))
	}
	if c.YAMLUnparseable > 0 {
		notChecked = append(notChecked, fmt.Sprintf("%d YAML file(s) that could not be parsed", c.YAMLUnparseable))
	}
	if c.HelmTotal > c.HelmScanned {
		notChecked = append(notChecked, fmt.Sprintf("%d Helm template(s) that do not render Kubernetes manifests", c.HelmTotal-c.HelmScanned))
	}
	for _, kind := range sortedKeys(c.NotCovered) {
		notChecked = append(notChecked, fmt.Sprintf("%d %s file(s): no rules yet", c.NotCovered[kind], kind))
	}
	return checked, notChecked
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// EncodeCoverage is the JSON stored on the Repository node.
func EncodeCoverage(c Coverage) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encoding coverage: %w", err)
	}
	return string(b), nil
}

// uncovered maps file names and extensions without rules to a display kind.
var (
	uncoveredNames = map[string]string{"Dockerfile": "Dockerfile", "Containerfile": "Dockerfile"}
	uncoveredExts  = map[string]string{
		".tf": "Terraform", ".sh": "Shell", ".bash": "Shell", ".java": "Java", ".kt": "Kotlin",
		".cs": "C#", ".rb": "Ruby", ".php": "PHP", ".rs": "Rust", ".c": "C", ".cpp": "C++",
		".swift": "Swift",
	}
	skipInventory = map[string]bool{
		".git": true, "node_modules": true, "vendor": true, ".nuxt": true, ".output": true,
		"dist": true, "testdata": true, ".terraform": true,
	}
)

// Inventory counts files under root of kinds no rule reads yet. It never
// follows symlinks and ignores unreadable entries.
func Inventory(root string) map[string]int {
	out := map[string]int{}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // unreadable entries are skipped, not fatal
		}
		if d.IsDir() {
			if p != root && skipInventory[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		name := d.Name()
		if kind, ok := uncoveredNames[name]; ok || strings.HasPrefix(name, "Dockerfile.") {
			if !ok {
				kind = "Dockerfile"
			}
			out[kind]++
			return nil
		}
		if kind, ok := uncoveredExts[strings.ToLower(filepath.Ext(name))]; ok {
			out[kind]++
		}
		return nil
	})
	return out
}
