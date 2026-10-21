// Package rules is the security rule catalogue. Every rule is a YAML file
// in code/ or config/ next to this file: what it detects, how severe it is,
// why it matters, how to fix it, and vulnerable -> fixed examples. The
// files are embedded into the binaries; the detectors in internal/ingest
// look rules up by ID, so the YAML is the single source of truth for
// everything a human reads.
package rules

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed code/*.yaml config/*.yaml
var files embed.FS

// Example is a vulnerable/fixed pair in one language.
type Example struct {
	Language   string `yaml:"language" json:"language"`
	Vulnerable string `yaml:"vulnerable" json:"vulnerable"`
	Fixed      string `yaml:"fixed" json:"fixed"`
}

// Rule is one security rule.
type Rule struct {
	ID         string    `yaml:"id" json:"rule"`
	Title      string    `yaml:"title" json:"title"`
	Severity   string    `yaml:"severity" json:"severity"`
	CWE        []string  `yaml:"cwe" json:"cwe"`
	Category   string    `yaml:"category" json:"category"`
	Languages  []string  `yaml:"languages" json:"languages"`
	Why        string    `yaml:"why" json:"why"`
	Fix        string    `yaml:"fix" json:"fix"`
	References []string  `yaml:"references" json:"references,omitempty"`
	Examples   []Example `yaml:"examples" json:"examples"`
	// File is the path of the rule file inside rules/.
	File string `yaml:"-" json:"file"`
}

var severities = []string{"info", "low", "medium", "high", "critical"}

var (
	once    sync.Once
	byID    map[string]Rule
	all     []Rule
	loadErr error
)

func load() {
	byID = map[string]Rule{}
	loadErr = fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(p) != ".yaml" {
			return err
		}
		data, err := files.ReadFile(p)
		if err != nil {
			return err
		}
		var r Rule
		dec := yaml.NewDecoder(strings.NewReader(string(data)))
		dec.KnownFields(true)
		if err := dec.Decode(&r); err != nil {
			return fmt.Errorf("rules/%s: %w", p, err)
		}
		r.File = "rules/" + p
		if err := validate(r, p); err != nil {
			return err
		}
		if _, dup := byID[r.ID]; dup {
			return fmt.Errorf("rules/%s: duplicate rule id %q", p, r.ID)
		}
		byID[r.ID] = r
		all = append(all, r)
		return nil
	})
	slices.SortFunc(all, func(a, b Rule) int {
		if d := slices.Index(severities, b.Severity) - slices.Index(severities, a.Severity); d != 0 {
			return d
		}
		return strings.Compare(a.ID, b.ID)
	})
}

func validate(r Rule, p string) error {
	want := strings.TrimSuffix(path.Base(p), ".yaml")
	switch {
	case r.ID != want:
		return fmt.Errorf("rules/%s: id %q must match the file name", p, r.ID)
	case r.Title == "" || r.Why == "" || r.Fix == "":
		return fmt.Errorf("rules/%s: title, why and fix are required", p)
	case !slices.Contains(severities, r.Severity):
		return fmt.Errorf("rules/%s: severity %q must be one of %v", p, r.Severity, severities)
	case r.Category != path.Dir(p):
		return fmt.Errorf("rules/%s: category %q must match its directory", p, r.Category)
	case len(r.Examples) == 0:
		return fmt.Errorf("rules/%s: at least one example is required", p)
	}
	for _, c := range r.CWE {
		if !strings.HasPrefix(c, "CWE-") {
			return fmt.Errorf("rules/%s: cwe %q must look like CWE-89", p, c)
		}
	}
	for i, e := range r.Examples {
		if e.Language == "" || e.Vulnerable == "" || e.Fixed == "" {
			return fmt.Errorf("rules/%s: example %d needs language, vulnerable and fixed", p, i+1)
		}
	}
	return nil
}

// Err reports a problem loading the embedded catalogue (tests assert nil).
func Err() error {
	once.Do(load)
	return loadErr
}

// All returns every rule, most severe first.
func All() []Rule {
	once.Do(load)
	return slices.Clone(all)
}

// Get returns the rule with id.
func Get(id string) (Rule, bool) {
	once.Do(load)
	r, ok := byID[id]
	return r, ok
}

// MustGet returns the rule with id or panics. Detectors call it with
// constant IDs, and TestEveryDetectorHasARule keeps them in sync, so a
// panic here is a programming error caught in tests.
func MustGet(id string) Rule {
	r, ok := Get(id)
	if !ok {
		panic(fmt.Sprintf("rules: no rule file for %q (add rules/<category>/%s.yaml)", id, id))
	}
	return r
}
