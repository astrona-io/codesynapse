{{/*
Helpers shared by the codesynapse chart and its store subcharts. Template
names are global in a release, so every subchart includes these. Each takes
its own chart's context: names and labels follow the release, and shared
settings come from .Values.global.
*/}}

{{/* Names: <release>-<component>, at most 63 characters. */}}
{{- define "cs.name" -}}
{{- $root := index . 0 -}}
{{- printf "%s-%s" $root.Release.Name (index . 1) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "cs.labels" -}}
{{- $root := index . 0 -}}
{{- /* The services' image.tag or the chart's appVersion; a store's image tag. */ -}}
{{- $tag := "" -}}
{{- if kindIs "map" $root.Values.image }}{{ $tag = $root.Values.image.tag }}
{{- else if kindIs "string" $root.Values.image }}{{ $tag = regexReplaceAll "^.*:" $root.Values.image "" }}{{ end -}}
app.kubernetes.io/name: {{ index . 1 }}
app.kubernetes.io/instance: {{ $root.Release.Name }}
app.kubernetes.io/part-of: codesynapse
app.kubernetes.io/version: {{ $tag | default $root.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ $root.Release.Service }}
helm.sh/chart: {{ printf "%s-%s" $root.Chart.Name $root.Chart.Version }}
{{- end -}}
{{- define "cs.selector" -}}
{{- $root := index . 0 -}}
app.kubernetes.io/name: {{ index . 1 }}
app.kubernetes.io/instance: {{ $root.Release.Name }}
{{- end -}}
{{- define "cs.pullSecrets" -}}
{{- with .Values.global.imagePullSecrets }}
imagePullSecrets:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end -}}
{{/* The one Secret with every credential; the chart never invents one. */}}
{{- define "cs.secret" -}}
{{- required "global.existingSecret is required: create a Secret with the keys listed in values.yaml" .Values.global.existingSecret -}}
{{- end -}}
{{- define "cs.secretEnv" -}}
{{- $root := index . 0 -}}
- name: {{ index . 1 }}
  valueFrom:
    secretKeyRef:
      name: {{ include "cs.secret" $root }}
      key: {{ index . 2 }}
      {{- if gt (len .) 3 }}
      optional: {{ index . 3 }}
      {{- end }}
{{- end -}}
{{/* Pods: non-root, no privilege escalation, read-only root, nothing extra. */}}
{{- define "cs.podSecurity" -}}
runAsNonRoot: true
runAsUser: {{ . }}
runAsGroup: {{ . }}
fsGroup: {{ . }}
seccompProfile:
  type: RuntimeDefault
{{- end -}}
{{- define "cs.containerSecurity" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities:
  drop: [ALL]
{{- end -}}
{{/* A store: one StatefulSet with its own volume and a Service. */}}
{{- define "cs.volumeClaim" -}}
{{- $root := index . 0 -}}{{- $v := index . 1 -}}
volumeClaimTemplates:
  - metadata:
      name: data
    spec:
      accessModes: [ReadWriteOnce]
      {{- with $v.storageClass }}
      storageClassName: {{ . }}
      {{- end }}
      resources:
        requests:
          storage: {{ $v.size }}
{{- end -}}

{{/* The pods' service account (created by the codesynapse chart). */}}
{{- define "cs.serviceAccount" -}}
{{- printf "%s-codesynapse" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* A store accepts only api, worker, fix-worker and mcp of the same release. Args: root, store. */}}
{{- define "cs.storePolicy" -}}
{{- $root := index . 0 -}}{{- $store := index . 1 -}}
{{- if $root.Values.global.networkPolicies }}
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ include "cs.name" (list $root $store) }}
  labels:
    {{- include "cs.labels" (list $root $store) | nindent 4 }}
spec:
  podSelector:
    matchLabels:
      {{- include "cs.selector" (list $root $store) | nindent 6 }}
  policyTypes: [Ingress]
  ingress:
    - from:
        {{- range $client := list "api" "worker" "fix-worker" "mcp" }}
        - podSelector:
            matchLabels:
              {{- include "cs.selector" (list $root $client) | nindent 14 }}
        {{- end }}
{{- end }}
{{- end -}}
