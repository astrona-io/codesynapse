package config

import (
	"context"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func TestExtract(t *testing.T) {
	g, err := Extract(context.Background(), "r", "testdata/repo")
	require.NoError(t, err)
	id := func(k model.NodeKind, l string) string { return model.NodeID("r", k, l) }
	has := func(from, to string, kind model.EdgeKind) bool {
		for _, e := range g.Edges() {
			if e.From == from && e.To == to && e.Kind == kind {
				return true
			}
		}
		return false
	}

	t.Run("kubernetes resources", func(t *testing.T) {
		n, ok := g.Node(id(model.KindConfigEntry, "k8s/app.yaml#Deployment/api"))
		require.True(t, ok)
		assert.Equal(t, n.StartLine, 1)
		assert.Equal(t, n.EndLine, 16, "ends before the --- separator")
		assert.Equal(t, n.Signature, "apps/v1")
		_, ok = g.Node(id(model.KindConfigEntry, "k8s/app.yaml#Secret/creds"))
		assert.True(t, ok)
	})

	t.Run("helm values and includes", func(t *testing.T) {
		tpl := id(model.KindTemplate, "chart/templates/deployment.yaml")
		assert.True(t, has(tpl, id(model.KindConfigEntry, "chart/values.yaml#image.repository"), model.EdgeUsesValue))
		assert.True(t, has(tpl, id(model.KindConfigEntry, "chart/values.yaml#service.port"), model.EdgeUsesValue))
		assert.True(t, has(tpl, id(model.KindTemplateDef, "chart:web.labels"), model.EdgeIncludes))
	})

	t.Run("go template define", func(t *testing.T) {
		def := id(model.KindTemplateDef, "greeting")
		assert.True(t, has(id(model.KindTemplate, "tmpl/mail.tmpl"), def, model.EdgeIncludes))
	})

	t.Run("security rules", func(t *testing.T) {
		rules := map[string]model.Node{}
		for _, n := range g.Nodes() {
			if n.Kind == model.KindFinding {
				rules[n.Rule] = n
			}
		}
		for _, r := range []string{
			"k8s-privileged", "k8s-host-namespace", "k8s-image-unpinned", "k8s-secret-in-repo",
			"yaml-hardcoded-secret", "gha-script-injection", "gha-pr-target-checkout", "gha-unpinned-action", "compose-privileged",
		} {
			assert.Contains(t, keys(rules), r)
		}
		assert.NotContains(t, rules["yaml-hardcoded-secret"].Summary, "hunter2")
		assert.Equal(t, rules["k8s-privileged"].Severity, "critical")
		// ${PG_PASSWORD} is a reference, not a secret; the chart image is templated.
		for _, n := range g.Nodes() {
			if n.Kind == model.KindFinding && n.File == "docker-compose.yml" {
				assert.NotEqual(t, n.Rule, "yaml-hardcoded-secret")
			}
			if n.Kind == model.KindFinding && n.File == "chart/templates/deployment.yaml" {
				t.Errorf("unexpected finding in templated manifest: %s", n.Rule)
			}
		}
	})

	t.Run("no values stored", func(t *testing.T) {
		for _, n := range g.Nodes() {
			assert.NotContains(t, n.Summary+n.Name+n.Signature, "abcdefgh12345")
		}
	})
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestIsReferenceKey(t *testing.T) {
	tests := []struct {
		key, value string
		ref        bool
	}{
		{"secretName", "keycloak-tls", true},
		{"existingSecret", "db-creds", true},
		{"tlsSecret", "wildcard-cert-x", true},
		{"secretKey", "password", true},
		{"password_key", "admin-pass", true},
		{"passwordRef", "x", true},
		{"apiKey", "sk_live_abcdef123", false},
		{"clientSecret", "Zx9#kLm2pQ", false},
		{"KC_DB_PASSWORD", "s3cr3t-pass-word", false},
		{"password", "hunter2-hunter2", false},
	}
	for _, tt := range tests {
		assert.Equal(t, isReferenceKey(tt.key, tt.value), tt.ref, tt.key)
	}
}
