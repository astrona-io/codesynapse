package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
	"gopkg.in/yaml.v3"
)

const deployment = `{{/*
  A chart's deployment.
*/}}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "app.fullname" . }}
  labels:
    {{- include "app.labels" . | nindent 4 }}
spec:
  template:
    spec:
      {{- if .Values.hostNetwork }}
      hostNetwork: {{ .Values.hostNetwork }}
      {{- end }}
      containers:
        - name: app
          image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
          securityContext:
            privileged: true
            {{- toYaml .Values.securityContext | nindent 12 }}
        - name: sidecar
          image: busybox:latest
      volumes:
        - name: host
          hostPath:
            path: /var/run
`

func TestHelmTemplatesAreScanned(t *testing.T) {
	root := t.TempDir()
	write := func(rel, s string) {
		p := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(s), 0o600))
	}
	write("chart/Chart.yaml", "apiVersion: v2\nname: app\nversion: 0.1.0\n")
	write("chart/values.yaml", "hostNetwork: false\n")
	write("chart/templates/deployment.yaml", deployment)
	write("chart/templates/_helpers.tpl", `{{- define "app.fullname" -}}{{ .Release.Name }}{{- end }}`)
	write("chart/templates/broken.yaml", "a: [\n{{ .x }}\n")

	g, err := Extract(context.Background(), "r", root)
	require.NoError(t, err)
	got := map[string]int{}
	for _, n := range g.Nodes() {
		if n.Kind == model.KindFinding && n.File == "chart/templates/deployment.yaml" {
			got[n.Rule] = n.StartLine
		}
	}
	assert.Equal(t, got["k8s-privileged"], 20, "literal privileged: true, at its own line")
	assert.Equal(t, got["k8s-image-unpinned"], 23, "busybox:latest is literal")
	assert.Equal(t, got["k8s-host-path"], 26)
	_, templatedNet := got["k8s-host-namespace"]
	assert.False(t, templatedNet, "hostNetwork comes from values: not judged")
	assert.Equal(t, len(got), 3, "the templated image is not flagged")

	scanned := map[string]string{}
	for _, n := range g.Nodes() {
		if n.Kind == model.KindTemplate {
			scanned[n.File] = n.Language
		}
	}
	assert.Equal(t, scanned["chart/templates/deployment.yaml"], "yaml")
	assert.Equal(t, scanned["chart/templates/broken.yaml"], "", "unreadable output is counted as not scanned")
}

func TestNeutralizeKeepsLines(t *testing.T) {
	out := string(neutralize([]byte(deployment)))
	assert.Equal(t, strings.Count(out, "\n"), strings.Count(deployment, "\n"))
	assert.False(t, strings.Contains(out, "{{"))
	assert.True(t, strings.Contains(out, `image: "__TPL__:__TPL__"`))
}

func FuzzNeutralize(f *testing.F) {
	for _, s := range []string{deployment, "{{", "}}", "a: {{ b }}\n{{/* x\ny */}}c: d", "{{- if x }}\n{{ end }}"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := neutralize([]byte(s))
		if strings.Count(string(out), "\n") != strings.Count(s, "\n") {
			t.Fatalf("line count changed")
		}
	})
}

func TestNeutralizeHelmIdioms(t *testing.T) {
	src := `metadata:
  labels: {{- include "common.labels" (dict "a" .Values.x) | nindent 4 }}
    app.kubernetes.io/part-of: valkey
  annotations: {{ toYaml .Values.annotations | nindent 4 }}
{{- $entries := dict
  "user" (dict "password" "{{ .password }}")
}}
spec:
  hostPID: true
  name: {{ printf "%s-x" (include "n" .) }}
`
	out := string(neutralize([]byte(src)))
	assert.Equal(t, strings.Count(out, "\n"), strings.Count(src, "\n"))
	assert.True(t, strings.Contains(out, "  labels:\n    app.kubernetes.io/part-of: valkey"), "a block-emitting action leaves the key empty")
	assert.True(t, strings.Contains(out, "  name: "+tplValue), "a value action becomes the placeholder")
	assert.False(t, strings.Contains(out, "password"), "a }} inside a quoted string does not end the action")
	var doc yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte(out), &doc))
}

func TestKubernetesRulesSkipSchemasAndTemplatedSecrets(t *testing.T) {
	root := t.TempDir()
	write := func(rel, s string) {
		p := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(s), 0o600))
	}
	write("chart/Chart.yaml", "apiVersion: v2\nname: c\nversion: 0.1.0\n")
	write("chart/templates/crd.yaml", "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: x\nspec:\n  versions:\n    - schema:\n        openAPIV3Schema:\n          properties:\n            hostPath:\n              type: string\n            privileged:\n              type: boolean\n")
	write("chart/templates/secret.yaml", "apiVersion: v1\nkind: Secret\nmetadata:\n  name: {{ .Release.Name }}\nstringData:\n  .htpasswd: |\n    {{- tpl .Values.htpasswd . | nindent 4 }}\n  token: {{ .Values.token }}\n")
	write("plain/secret.yaml", "apiVersion: v1\nkind: Secret\nmetadata:\n  name: s\nstringData:\n  a: real-literal\n  b: \"${FROM_ENV}\"\n")
	g, err := Extract(context.Background(), "r", root)
	require.NoError(t, err)
	var got []string
	for _, n := range g.Nodes() {
		if n.Kind == model.KindFinding && strings.HasPrefix(n.Rule, "k8s-") {
			got = append(got, n.File+" "+n.Rule+" "+n.Summary)
		}
	}
	require.Len(t, got, 1, strings.Join(got, "\n"))
	assert.True(t, strings.HasPrefix(got[0], "plain/secret.yaml k8s-secret-in-repo Secret s has 1 literal stringData keys"), got[0])
}
