// Package config loads bounded, untrusted repository configuration.
package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/astrona-io/codesynapse/internal/redact"
	"github.com/astrona-io/codesynapse/rules"
	"gopkg.in/yaml.v3"
)

// Configuration limits bound parsing and matching work.
const (
	Filename        = ".codesynapse.yaml"
	MaxBytes        = 64 << 10
	MaxPatterns     = 256
	MaxPatternBytes = 512
)

// RepoConfig contains requests, not authority to suppress findings or run models.
type RepoConfig struct {
	Version    int              `yaml:"version" json:"version"`
	Ignore     IgnoreConfig     `yaml:"ignore" json:"ignore"`
	Scanners   ScannerConfig    `yaml:"scanners" json:"scanners"`
	Annotation AnnotationConfig `yaml:"annotation" json:"annotation"`
}

// IgnoreConfig requests graph ingestion exclusions.
type IgnoreConfig struct {
	Paths []string `yaml:"paths" json:"paths"`
}

// ScannerConfig groups domain-specific scanner requests.
type ScannerConfig struct {
	Kubernetes KubernetesConfig `yaml:"kubernetes" json:"kubernetes"`
	Security   SecurityConfig   `yaml:"security" json:"security"`
}

// KubernetesConfig controls Kubernetes classification independently of generic YAML checks.
type KubernetesConfig struct {
	Enabled     *bool    `yaml:"enabled" json:"enabled,omitempty"`
	IgnorePaths []string `yaml:"ignore_paths" json:"ignore_paths"`
}

// SecurityConfig contains requests requiring separate trusted suppression approval.
type SecurityConfig struct {
	IgnoreRules []RuleRequest `yaml:"ignore_rules" json:"ignore_rules"`
	// InlineIgnores asks to honour codesynapse:ignore comments next to findings.
	// Omitted when false so earlier digests and approvals stay valid.
	InlineIgnores bool `yaml:"inline_ignores" json:"inline_ignores,omitempty"`
}

// RuleRequest names one canonical security rule and its requested path exceptions.
type RuleRequest struct {
	RuleID string   `yaml:"rule_id" json:"rule_id"`
	Paths  []string `yaml:"paths" json:"paths"`
}

// AnnotationConfig requests local annotation within trusted service limits.
type AnnotationConfig struct {
	Enabled       *bool    `yaml:"enabled" json:"enabled,omitempty"`
	ProviderTask  string   `yaml:"provider_task" json:"provider_task"`
	MaxBatchSize  int      `yaml:"max_batch_size" json:"max_batch_size"`
	IgnorePaths   []string `yaml:"ignore_paths" json:"ignore_paths"`
	TaxonomyHints []string `yaml:"taxonomy_hints" json:"taxonomy_hints"`
}

// Diagnostic uses fixed codes and schema fields, never repository text.
type Diagnostic struct {
	Code  string `json:"code"`
	Field string `json:"field"`
	Line  int    `json:"line"`
}

type suppression struct {
	rule  string
	paths []pattern
}

// Policy is immutable after validation; callers receive copies of its data.
type Policy struct {
	suppressions                   []suppression
	config                         RepoConfig
	digest                         string
	present                        bool
	ignore, kubernetes, annotation []pattern
	diagnostics                    []Diagnostic
}

type policyKey struct{}

// WithPolicy shares one validated policy across ingestion stages.
func WithPolicy(ctx context.Context, p *Policy) context.Context {
	return context.WithValue(ctx, policyKey{}, p)
}

// FromContext returns nil when no repository policy was loaded.
func FromContext(ctx context.Context) *Policy { p, _ := ctx.Value(policyKey{}).(*Policy); return p }

// Present distinguishes an absent file from an explicit policy.
func (p *Policy) Present() bool { return p != nil && p.present }

// Digest identifies the normalized configuration without exposing its contents.
func (p *Policy) Digest() string {
	if p == nil {
		return ""
	}
	return p.digest
}

// Diagnostics returns validation notices as an independent copy.
func (p *Policy) Diagnostics() []Diagnostic {
	if p == nil {
		return nil
	}
	return slices.Clone(p.diagnostics)
}

// Config returns a deep copy of the normalized repository requests.
func (p *Policy) Config() RepoConfig {
	if p == nil {
		return RepoConfig{Version: 1}
	}
	c := p.config
	c.Ignore.Paths = slices.Clone(c.Ignore.Paths)
	c.Scanners.Kubernetes.IgnorePaths = slices.Clone(c.Scanners.Kubernetes.IgnorePaths)
	c.Scanners.Kubernetes.Enabled = copyBool(c.Scanners.Kubernetes.Enabled)
	c.Scanners.Security.IgnoreRules = slices.Clone(c.Scanners.Security.IgnoreRules)
	for i := range c.Scanners.Security.IgnoreRules {
		c.Scanners.Security.IgnoreRules[i].Paths = slices.Clone(c.Scanners.Security.IgnoreRules[i].Paths)
	}
	c.Annotation.Enabled = copyBool(c.Annotation.Enabled)
	c.Annotation.IgnorePaths = slices.Clone(c.Annotation.IgnorePaths)
	c.Annotation.TaxonomyHints = slices.Clone(c.Annotation.TaxonomyHints)
	return c
}
func copyBool(v *bool) *bool {
	if v == nil {
		return nil
	}
	b := *v
	return &b
}

// Load rejects a replaced symlink or special file before reading any bytes.
func Load(ctx context.Context, root string) (*Policy, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open repository root: %w", err)
	}
	defer func() { _ = dir.Close() }()
	info, err := dir.Lstat(Filename)
	if errors.Is(err, os.ErrNotExist) {
		return &Policy{config: RepoConfig{Version: 1}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat repository configuration: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > MaxBytes {
		return nil, invalid("document", 1, "expected a regular file of at most 64 KiB")
	}
	f, err := openConfig(dir)
	if err != nil {
		return nil, fmt.Errorf("open repository configuration: %w", err)
	}
	defer func() { _ = f.Close() }()
	current, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat opened repository configuration: %w", err)
	}
	if !current.Mode().IsRegular() {
		return nil, invalid("document", 1, "expected a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read repository configuration: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse validates one complete version 1 document without retaining its source.
func Parse(data []byte) (*Policy, error) {
	if len(data) > MaxBytes {
		return nil, invalid("document", 1, "exceeds 64 KiB")
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		return nil, invalid("document", 1, "invalid YAML")
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, invalid("document", 1, "expected exactly one document")
	}
	if len(doc.Content) != 1 {
		return nil, invalid("document", 1, "expected a mapping")
	}
	count := 0
	if err := checkNode(doc.Content[0], "", 0, &count); err != nil {
		return nil, err
	}
	var c RepoConfig
	if err := doc.Decode(&c); err != nil {
		return nil, invalid("document", 1, "invalid field type")
	}
	if c.Version != 1 {
		return nil, invalid("version", fieldLine(&doc, "version"), "only version 1 is supported")
	}
	p := &Policy{config: c, present: true}
	if err := p.validate(&doc); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(p.config)
	if err != nil {
		return nil, fmt.Errorf("encode validated repository policy: %w", err)
	}
	sum := sha256.Sum256(canonical)
	p.digest = hex.EncodeToString(sum[:])
	return p, nil
}

func (p *Policy) validate(doc *yaml.Node) error {
	c := &p.config
	total := 0
	compile := func(field string, paths []string) ([]pattern, error) {
		total += len(paths)
		if total > MaxPatterns {
			return nil, invalid(field, fieldLine(doc, field), "exceeds 256 patterns")
		}
		out := make([]pattern, 0, len(paths))
		for _, s := range paths {
			pat, err := compilePattern(s)
			if err != nil {
				return nil, invalid(field, fieldLine(doc, field), "invalid root-relative glob")
			}
			out = append(out, pat)
		}
		return out, nil
	}
	var err error
	if p.ignore, err = compile("ignore.paths", c.Ignore.Paths); err != nil {
		return err
	}
	if p.kubernetes, err = compile("scanners.kubernetes.ignore_paths", c.Scanners.Kubernetes.IgnorePaths); err != nil {
		return err
	}
	if p.annotation, err = compile("annotation.ignore_paths", c.Annotation.IgnorePaths); err != nil {
		return err
	}
	for i := range c.Scanners.Security.IgnoreRules {
		r := &c.Scanners.Security.IgnoreRules[i]
		if r.RuleID == "sec-yaml-plain-secret" {
			r.RuleID = "yaml-hardcoded-secret"
			p.diagnostics = append(p.diagnostics, Diagnostic{"rule_alias_normalized", "scanners.security.ignore_rules", fieldLine(doc, "scanners.security.ignore_rules")})
		}
		if _, ok := rules.Get(r.RuleID); !ok {
			return invalid("scanners.security.ignore_rules", fieldLine(doc, "scanners.security.ignore_rules"), "unknown rule identifier")
		}
		if len(r.Paths) == 0 {
			return invalid("scanners.security.ignore_rules", fieldLine(doc, "scanners.security.ignore_rules"), "each request needs at least one path")
		}
		patterns, err := compile("scanners.security.ignore_rules", r.Paths)
		if err != nil {
			return err
		}
		p.suppressions = append(p.suppressions, suppression{r.RuleID, patterns})
	}
	a := &c.Annotation
	if a.ProviderTask == "" {
		a.ProviderTask = "code-annotation"
	}
	if a.ProviderTask != "code-annotation" {
		return invalid("annotation.provider_task", fieldLine(doc, "annotation.provider_task"), "task is not allowed")
	}
	if a.MaxBatchSize == 0 {
		if fieldLine(doc, "annotation.max_batch_size") != 0 {
			return invalid("annotation.max_batch_size", fieldLine(doc, "annotation.max_batch_size"), "must be between 1 and 100")
		}
		a.MaxBatchSize = 20
	}
	if a.MaxBatchSize < 1 || a.MaxBatchSize > 100 {
		return invalid("annotation.max_batch_size", fieldLine(doc, "annotation.max_batch_size"), "must be between 1 and 100")
	}
	if len(a.TaxonomyHints) > 32 {
		return invalid("annotation.taxonomy_hints", fieldLine(doc, "annotation.taxonomy_hints"), "exceeds 32 hints")
	}
	for i, s := range a.TaxonomyHints {
		if len(s) == 0 || len(s) > 64 || !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl) {
			return invalid("annotation.taxonomy_hints", fieldLine(doc, "annotation.taxonomy_hints"), "expected nonempty text of at most 64 bytes")
		}
		a.TaxonomyHints[i] = redact.Free(s)
	}
	requests := []struct {
		field     string
		requested bool
	}{
		{"ignore.paths", len(p.ignore) > 0},
		{"scanners.kubernetes", len(p.kubernetes) > 0 || c.Scanners.Kubernetes.Enabled != nil && !*c.Scanners.Kubernetes.Enabled},
		{"scanners.security.ignore_rules", len(c.Scanners.Security.IgnoreRules) > 0},
		{"scanners.security.inline_ignores", c.Scanners.Security.InlineIgnores},
		{"annotation.enabled", a.Enabled != nil && *a.Enabled},
	}
	for _, r := range requests {
		if r.requested {
			p.diagnostics = append(p.diagnostics, Diagnostic{"approval_required", r.field, fieldLine(doc, r.field)})
		}
	}
	return nil
}

func invalid(field string, line int, reason string) error {
	return fmt.Errorf("repository configuration %s at line %d: %s", field, line, reason)
}

// ParseCanonicalJSON restores a previously validated Config without accepting YAML nulls.
// It rejects configurations whose defaults, rule aliases or redaction are not normalized.
func ParseCanonicalJSON(data []byte) (*Policy, error) {
	if len(data) > 2*MaxBytes {
		return nil, invalid("document", 1, "canonical configuration is too large")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c RepoConfig
	if err := dec.Decode(&c); err != nil {
		return nil, invalid("document", 1, "invalid canonical configuration")
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, invalid("document", 1, "expected exactly one JSON value")
	}
	before, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil || len(doc.Content) != 1 {
		return nil, invalid("document", 1, "invalid canonical configuration")
	}
	if err := removeCanonicalNulls(doc.Content[0]); err != nil {
		return nil, err
	}
	count := 0
	if err := checkNode(doc.Content[0], "", 0, &count); err != nil {
		return nil, err
	}
	if c.Version != 1 {
		return nil, invalid("version", 1, "only version 1 is supported")
	}
	p := &Policy{config: c, present: true}
	if err := p.validate(&doc); err != nil {
		return nil, err
	}
	after, err := json.Marshal(p.config)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(before, after) {
		return nil, invalid("document", 1, "configuration is not normalized")
	}
	sum := sha256.Sum256(after)
	p.digest = hex.EncodeToString(sum[:])
	return p, nil
}

func removeCanonicalNulls(n *yaml.Node) error {
	if n.Kind == yaml.MappingNode {
		kept := n.Content[:0]
		seen := make(map[string]bool)
		for i := 0; i < len(n.Content); i += 2 {
			key, value := n.Content[i], n.Content[i+1]
			if seen[key.Value] {
				return invalid("document", key.Line, "duplicate field")
			}
			seen[key.Value] = true
			if value.Tag == "!!null" {
				continue
			}
			if err := removeCanonicalNulls(value); err != nil {
				return err
			}
			kept = append(kept, key, value)
		}
		n.Content = kept
	} else if n.Kind == yaml.SequenceNode {
		for _, child := range n.Content {
			if err := removeCanonicalNulls(child); err != nil {
				return err
			}
		}
	}
	return nil
}
