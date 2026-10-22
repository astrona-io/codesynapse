package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestDefaultsAndImmutability(t *testing.T) {
	absent, err := Load(context.Background(), t.TempDir())
	require.NoError(t, err)
	assert.False(t, absent.Present())
	assert.True(t, absent.KubernetesAllowed("app.yaml"))
	assert.False(t, absent.AnnotationAllowed("app.go"))
	assert.False(t, absent.PathAllowed(Filename))
	p, err := Parse([]byte("version: 1\nannotation:\n  enabled: true\n  taxonomy_hints: [billing]\n  ignore_paths: ['**/*.pb.go']\n"))
	require.NoError(t, err)
	c := p.Config()
	assert.Equal(t, c.Annotation.ProviderTask, "code-annotation")
	assert.Equal(t, c.Annotation.MaxBatchSize, 20)
	assert.True(t, p.AnnotationAllowed("app.go"))
	assert.False(t, p.AnnotationAllowed("app.pb.go"))
	*c.Annotation.Enabled = false
	c.Annotation.IgnorePaths[0] = "**"
	c.Annotation.TaxonomyHints[0] = "changed"
	assert.True(t, p.AnnotationAllowed("app.go"))
	assert.Equal(t, p.Config().Annotation.TaxonomyHints[0], "billing")
	diagnostics := p.Diagnostics()
	diagnostics[0].Code = "changed"
	assert.Equal(t, p.Diagnostics()[0].Code, "approval_required")
	assert.Equal(t, FromContext(WithPolicy(context.Background(), p)), p)
	assert.Equal(t, len(p.Digest()), 64)
}

func TestStrictDocuments(t *testing.T) {
	cases := map[string]string{
		"empty": "", "missing_version": "ignore: {}", "version": "version: 2",
		"duplicate": "version: 1\nversion: 1", "nested_duplicate": "version: 1\nannotation:\n  enabled: true\n  enabled: false",
		"unknown": "version: 1\nsecret-dont-echo: private-text", "nested_unknown": "version: 1\nannotation:\n  credentials: private-text",
		"multiple": "version: 1\n---\nversion: 1", "empty_second": "version: 1\n---",
		"alias":      "version: 1\nignore: &x {paths: []}\nannotation: *x",
		"merge":      "version: 1\nignore: {<<: {paths: []}}",
		"custom_tag": "version: 1\nannotation: !execute {}", "scalar_tag": "version: 1\nannotation: {enabled: !execute true}",
		"wrong_bool": "version: 1\nannotation: {enabled: yes}", "null": "version: 1\nannotation: null",
		"wrong_list": "version: 1\nignore: {paths: true}", "list_value": "version: 1\nignore: {paths: [12]}",
		"task":       "version: 1\nannotation: {provider_task: cloud-private-text}",
		"zero_batch": "version: 1\nannotation: {max_batch_size: 0}", "negative_batch": "version: 1\nannotation: {max_batch_size: -1}", "large_batch": "version: 1\nannotation: {max_batch_size: 101}",
		"rule":             "version: 1\nscanners: {security: {ignore_rules: [{rule_id: private-text, paths: [x]}]}}",
		"empty_rule_paths": "version: 1\nscanners: {security: {ignore_rules: [{rule_id: yaml-hardcoded-secret, paths: []}]}}",
		"large_hint":       "version: 1\nannotation: {taxonomy_hints: ['" + strings.Repeat("x", 65) + "']}",
		"large_document":   strings.Repeat(" ", MaxBytes+1),
		"many_patterns":    "version: 1\nignore: {paths: [" + strings.Repeat("x,", MaxPatterns) + "x]}",
		"many_hints":       "version: 1\nannotation: {taxonomy_hints: [" + strings.Repeat("x,", 32) + "x]}",
		"deep":             "version: 1\nignore: " + strings.Repeat("[", 1000) + "0" + strings.Repeat("]", 1000),
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(src))
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "private-text")
			assert.NotContains(t, err.Error(), "secret-dont-echo")
		})
	}
}
func TestRuleAliasAndDigest(t *testing.T) {
	alias := "version: 1\nscanners:\n  security:\n    ignore_rules:\n      - rule_id: sec-yaml-plain-secret\n        paths: ['test/*.yaml']\n"
	p, err := Parse([]byte(alias))
	require.NoError(t, err)
	assert.Equal(t, p.Config().Scanners.Security.IgnoreRules[0].RuleID, "yaml-hardcoded-secret")
	assert.Equal(t, p.Diagnostics()[0].Code, "rule_alias_normalized")
	assert.True(t, p.Suppresses("yaml-hardcoded-secret", "test/fixture.yaml"))
	assert.False(t, p.Suppresses("yaml-hardcoded-secret", "app.yaml"))
	assert.False(t, p.Suppresses("yaml-hardcoded-secret", "../test/fixture.yaml"))
	q, err := Parse([]byte(strings.ReplaceAll(alias, "sec-yaml-plain-secret", "yaml-hardcoded-secret")))
	require.NoError(t, err)
	assert.Equal(t, p.Digest(), q.Digest())
	c := p.Config()
	c.Scanners.Security.IgnoreRules[0].Paths[0] = "**"
	assert.False(t, p.Suppresses("yaml-hardcoded-secret", "app.yaml"))
}
func TestExplicitScannerAndAnnotationBooleans(t *testing.T) {
	p, err := Parse([]byte("version: 1\nscanners: {kubernetes: {enabled: false}}\nannotation: {enabled: false}"))
	require.NoError(t, err)
	assert.False(t, p.KubernetesAllowed("deploy.yaml"))
	assert.False(t, p.AnnotationAllowed("main.go"))
	assert.True(t, p.RequiresApproval())
	assert.True(t, p.PathAllowed("deploy.yaml"))
	assert.False(t, *p.Config().Scanners.Kubernetes.Enabled)
	q, err := Parse([]byte("version: 1"))
	require.NoError(t, err)
	assert.True(t, q.Config().Scanners.Kubernetes.Enabled == nil)
	assert.False(t, q.RequiresApproval())
}
func TestLoadBoundaries(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, Filename)
	require.NoError(t, os.WriteFile(file, []byte("version: 1"), 0600))
	p, err := Load(context.Background(), root)
	require.NoError(t, err)
	assert.True(t, p.Present())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = Load(ctx, root)
	assert.ErrorIs(t, err, context.Canceled)
	require.NoError(t, os.Remove(file))
	target := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(target, []byte("version: 1"), 0600))
	require.NoError(t, os.Symlink(target, file))
	_, err = Load(context.Background(), root)
	assert.Error(t, err)
	require.NoError(t, os.Remove(file))
	require.NoError(t, os.Mkdir(file, 0700))
	_, err = Load(context.Background(), root)
	assert.Error(t, err)
}
func FuzzParse(f *testing.F) {
	f.Add([]byte("version: 1"))
	f.Add([]byte("version: 1\nignore: {paths: ['**/*.go']}"))
	f.Add([]byte("version: 1\nannotation: &x {enabled: true}"))
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := Parse(data)
		if err == nil {
			assert.True(t, p.Present())
			assert.Equal(t, p.Config().Version, 1)
			assert.Equal(t, len(p.Digest()), 64)
		}
	})
}

func TestCanonicalRoundTrip(t *testing.T) {
	for _, source := range []string{
		"version: 1",
		"version: 1\nignore: {paths: ['vendor/**']}",
		"version: 1\nannotation: {enabled: true, taxonomy_hints: [billing]}",
		"version: 1\nscanners: {security: {ignore_rules: [{rule_id: sec-yaml-plain-secret, paths: ['test/**']}]}}",
	} {
		p, err := Parse([]byte(source))
		require.NoError(t, err)
		raw, err := json.Marshal(p.Config())
		require.NoError(t, err)
		restored, err := ParseCanonicalJSON(raw)
		require.NoError(t, err)
		assert.Equal(t, restored.Digest(), p.Digest())
		assert.Equal(t, restored.Config(), p.Config())
	}
	for _, raw := range []string{`null`, `{"version":1}`, `{"version":1,"unknown":null}`, `{"version":1,"version":1}`, `{"version":1} {}`} {
		_, err := ParseCanonicalJSON([]byte(raw))
		assert.Error(t, err)
	}
}
