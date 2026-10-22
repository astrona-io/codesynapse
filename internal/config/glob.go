package config

import (
	"errors"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

type pattern struct{ parts []string }

func compilePattern(s string) (pattern, error) {
	if len(s) > MaxPatternBytes || !validPath(s) || strings.ContainsAny(s, "!{}[]") {
		return pattern{}, errors.New("invalid glob")
	}
	parts := strings.Split(s, "/")
	for _, p := range parts {
		if strings.Contains(p, "**") && p != "**" {
			return pattern{}, errors.New("double star must be a whole component")
		}
	}
	return pattern{parts}, nil
}
func validPath(s string) bool {
	if s == "" || len(s) > 4096 || !utf8.ValidString(s) || strings.ContainsAny(s, "\\:") || strings.ContainsFunc(s, unicode.IsControl) {
		return false
	}
	parts := strings.Split(s, "/")
	if len(parts) > 256 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}
func (p pattern) match(parts []string) bool {
	// Each state is visited once, including repeated double-star components.
	prev := make([]bool, len(parts)+1)
	next := make([]bool, len(parts)+1)
	prev[0] = true
	for _, component := range p.parts {
		clear(next)
		if component == "**" {
			next[0] = prev[0]
			for j := 1; j <= len(parts); j++ {
				next[j] = prev[j] || next[j-1]
			}
		} else {
			for j := 1; j <= len(parts); j++ {
				if prev[j-1] {
					next[j], _ = path.Match(component, parts[j-1])
				}
			}
		}
		prev, next = next, prev
	}
	return prev[len(parts)]
}
func matches(patterns []pattern, rel string) bool {
	parts := strings.Split(rel, "/")
	for _, p := range patterns {
		if p.match(parts) {
			return true
		}
	}
	return false
}

// Ignored includes unsafe paths and the configuration file itself.
func (p *Policy) Ignored(rel string) bool { return !p.PathAllowed(rel) }

// PathAllowed evaluates a slash-separated path relative to the checkout root.
func (p *Policy) PathAllowed(rel string) bool {
	if !validPath(rel) || rel == Filename {
		return false
	}
	return p == nil || !matches(p.ignore, rel)
}

// PruneDir returns true only when a terminal double star covers every descendant.
func (p *Policy) PruneDir(rel string) bool {
	if !validPath(rel) {
		return true
	}
	if p == nil {
		return false
	}
	for _, pat := range p.ignore {
		if pat.parts[len(pat.parts)-1] == "**" && pat.match(strings.Split(rel, "/")) {
			return true
		}
	}
	return false
}

// KubernetesAllowed never disables generic YAML or secret checks.
func (p *Policy) KubernetesAllowed(rel string) bool {
	if !p.PathAllowed(rel) {
		return false
	}
	if p == nil {
		return true
	}
	return (p.config.Scanners.Kubernetes.Enabled == nil || *p.config.Scanners.Kubernetes.Enabled) && !matches(p.kubernetes, rel)
}

// AnnotationAllowed reports requested eligibility; callers still authorize model work.
func (p *Policy) AnnotationAllowed(rel string) bool {
	return p != nil && p.config.Annotation.Enabled != nil && *p.config.Annotation.Enabled && p.PathAllowed(rel) && !matches(p.annotation, rel)
}

// RequiresApproval identifies graph and scanner coverage changes requiring trusted approval.
func (p *Policy) RequiresApproval() bool {
	return p != nil && (len(p.ignore) > 0 || len(p.kubernetes) > 0 || p.config.Scanners.Kubernetes.Enabled != nil && !*p.config.Scanners.Kubernetes.Enabled)
}

// WalkEntry applies the same relative-path boundary to every extractor walker.
func (p *Policy) WalkEntry(root, filename string, entry fs.DirEntry) (bool, error) {
	if filename == root {
		return true, nil
	}
	rel, err := filepath.Rel(root, filename)
	if err != nil {
		return false, err
	}
	rel = filepath.ToSlash(rel)
	if entry.IsDir() {
		if p.PruneDir(rel) {
			return false, filepath.SkipDir
		}
		return true, nil
	}
	if !entry.Type().IsRegular() {
		return false, nil
	}
	return p.PathAllowed(rel), nil
}

// Suppresses reports a requested suppression. The caller must verify trusted approval first.
func (p *Policy) Suppresses(rule, rel string) bool {
	if p == nil || !validPath(rel) {
		return false
	}
	for _, request := range p.suppressions {
		if request.rule == rule && matches(request.paths, rel) {
			return true
		}
	}
	return false
}
