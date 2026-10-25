{{- define "cs.image" -}}
{{- $root := index . 0 -}}
{{- printf "%s/codesynapse-%s:%s" $root.Values.image.registry (index . 1) ($root.Values.image.tag | default $root.Chart.AppVersion) -}}
{{- end -}}






{{/* Store addresses: in-chart service or the external value. */}}
{{- define "cs.hugegraphURL" -}}
{{- if .Values.hugegraph.enabled }}http://{{ include "cs.name" (list . "hugegraph") }}:8080{{ else }}{{ required "hugegraph.externalURL is required when hugegraph.enabled is false" .Values.hugegraph.externalURL }}{{ end -}}
{{- end -}}
{{- define "cs.milvusURL" -}}
{{- if .Values.milvus.enabled }}http://{{ include "cs.name" (list . "milvus") }}:19530{{ else }}{{ .Values.milvus.externalURL }}{{ end -}}
{{- end -}}
{{- define "cs.metricsURL" -}}
{{- if .Values.victoriametrics.enabled }}http://{{ include "cs.name" (list . "victoriametrics") }}:8428{{ else }}{{ .Values.victoriametrics.externalURL }}{{ end -}}
{{- end -}}
{{- define "cs.tracesURL" -}}
{{- if .Values.victoriatraces.enabled }}http://{{ include "cs.name" (list . "victoriatraces") }}:10428/insert/opentelemetry/v1/traces{{ else }}{{ .Values.victoriatraces.externalURL }}{{ end -}}
{{- end -}}

{{/* Environment shared by api, worker and mcp: stores and telemetry. */}}
{{- define "cs.storeEnv" -}}
- {name: CODESYNAPSE_HUGEGRAPH_URL, value: {{ include "cs.hugegraphURL" . | quote }}}
- {name: CODESYNAPSE_HUGEGRAPH_USER, value: admin}
{{ include "cs.secretEnv" (list . "CODESYNAPSE_HUGEGRAPH_PASSWORD" "HUGEGRAPH_PASSWORD") }}
- {name: CODESYNAPSE_HUGEGRAPH_MAX_REQUEST_BYTES, value: "8388608"}
- {name: CODESYNAPSE_HUGEGRAPH_TX_CAPACITY, value: {{ int64 .Values.hugegraph.txCapacity | quote }}}
- {name: CODESYNAPSE_MILVUS_URL, value: {{ include "cs.milvusURL" . | quote }}}
{{ include "cs.secretEnv" (list . "CODESYNAPSE_MILVUS_TOKEN" "MILVUS_TOKEN" true) }}
- {name: CODESYNAPSE_METRICS_URL, value: {{ include "cs.metricsURL" . | quote }}}
- {name: CODESYNAPSE_METRICS_USER, value: codesynapse}
{{ include "cs.secretEnv" (list . "CODESYNAPSE_METRICS_PASSWORD" "METRICS_PASSWORD") }}
- {name: CODESYNAPSE_TRACES_URL, value: {{ include "cs.tracesURL" . | quote }}}
- {name: CODESYNAPSE_TRACES_USER, value: codesynapse}
{{ include "cs.secretEnv" (list . "CODESYNAPSE_TRACES_PASSWORD" "METRICS_PASSWORD") }}
- {name: CODESYNAPSE_TRACES_SAMPLE, value: {{ .Values.traces.sample | quote }}}
- {name: CODESYNAPSE_ENVIRONMENT, value: {{ .Values.environment | quote }}}
{{- if .Values.postgresql.enabled }}
{{ include "cs.secretEnv" (list . "POSTGRES_PASSWORD" "POSTGRES_PASSWORD") }}
- name: CODESYNAPSE_DATABASE_URL
  value: "postgres://codesynapse:$(POSTGRES_PASSWORD)@{{ include "cs.name" (list . "postgresql") }}:5432/codesynapse?sslmode=disable"
{{- else }}
{{ include "cs.secretEnv" (list . "CODESYNAPSE_DATABASE_URL" "CODESYNAPSE_DATABASE_URL") }}
{{- end }}
{{- end -}}

{{/* GOMEMLIMIT a little under the memory limit, so Go collects before the kernel kills. */}}
{{- define "cs.gomemlimit" -}}
{{- $lim := .limits.memory | toString -}}
{{- if hasSuffix "Gi" $lim -}}
{{- printf "%dMiB" (div (mul (trimSuffix "Gi" $lim | atoi) 1024 90) 100) -}}
{{- else if hasSuffix "Mi" $lim -}}
{{- printf "%dMiB" (div (mul (trimSuffix "Mi" $lim | atoi) 90) 100) -}}
{{- end -}}
{{- end -}}

{{/*
Environment of a process that runs jobs: the API (it still runs them when no
worker does) and the workers. Args: root, push ("true" or "false": whether
this process may push fix branches). Only the fix workers get "true"; the
ingest and ai workers parse untrusted repositories and never hold it.
*/}}
{{- define "cs.jobEnv" -}}
{{- $root := index . 0 -}}
{{ include "cs.storeEnv" $root }}
{{ include "cs.secretEnv" (list $root "CODESYNAPSE_SECRET_KEY" "SECRET_KEY") }}
{{ include "cs.secretEnv" (list $root "CODESYNAPSE_GIT_TOKENS" "CODESYNAPSE_GIT_TOKENS" true) }}
{{ include "cs.secretEnv" (list $root "ANTHROPIC_API_KEY" "ANTHROPIC_API_KEY" true) }}
{{ include "cs.secretEnv" (list $root "OPENROUTER_API_KEY" "OPENROUTER_API_KEY" true) }}
- {name: CODESYNAPSE_TRUSTED_PROXIES, value: {{ $root.Values.trustedProxies | quote }}}
- {name: CODESYNAPSE_PUBLIC_URL, value: {{ $root.Values.publicURL | quote }}}
- {name: CODESYNAPSE_PUBLIC_MCP_URL, value: {{ $root.Values.publicMCPURL | quote }}}
- {name: CODESYNAPSE_PUBLIC_API_URL, value: {{ $root.Values.publicAPIURL | quote }}}
- {name: CODESYNAPSE_SIGNUP_OPEN, value: {{ $root.Values.signupOpen | quote }}}
- {name: CODESYNAPSE_COOKIE_SECURE, value: {{ $root.Values.cookieSecure | quote }}}
- {name: CODESYNAPSE_AUDIT_RETENTION_DAYS, value: {{ $root.Values.auditRetentionDays | quote }}}
- {name: CODESYNAPSE_ALLOW_NETWORK, value: {{ $root.Values.features.allowNetwork | quote }}}
- {name: CODESYNAPSE_VULN_SCAN, value: {{ $root.Values.features.vulnScan | quote }}}
- {name: CODESYNAPSE_LICENSE_LOOKUP, value: {{ $root.Values.features.licenseLookup | quote }}}
- {name: CODESYNAPSE_INGEST_DATAFLOW, value: {{ $root.Values.features.dataflow | quote }}}
- {name: CODESYNAPSE_ALLOW_PUSH, value: {{ index . 1 | quote }}}
- {name: CODESYNAPSE_ALLOW_PRIVATE_GIT, value: {{ $root.Values.features.allowPrivateGit | quote }}}
- {name: CODESYNAPSE_WORKSPACE, value: /workspace}
- {name: CODESYNAPSE_LLM_CONFIG, value: /config/llm.yaml}
{{- end -}}

{{/*
Environment of the API process. On top of the job environment it holds what
only the API uses: the service token (workers serve no HTTP) and the first
administrator's password. Workers never get either.
*/}}
{{- define "cs.apiEnv" -}}
{{ include "cs.jobEnv" (list . .Values.features.allowPush) }}
{{ include "cs.secretEnv" (list . "CODESYNAPSE_API_TOKEN" "CODESYNAPSE_API_TOKEN") }}
{{- if .Values.admin.enabled }}
- name: CODESYNAPSE_ADMIN_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ .Values.admin.existingSecret | default (include "cs.name" (list . "admin")) }}
      key: CODESYNAPSE_ADMIN_PASSWORD
{{- else }}
- {name: CODESYNAPSE_AUTO_ADMIN, value: "false"}
{{- end }}
{{- end -}}


{{/* Replicas, unless an autoscaler owns them. Args: root, component. */}}
{{- define "cs.replicas" -}}
{{- $v := index (index . 0).Values (index . 1) -}}
{{- if not (and $v.autoscaling $v.autoscaling.enabled) -}}
replicas: {{ $v.replicas }}
{{- end -}}
{{- end -}}

{{/* Prefer one pod per node, and spread over zones, when the cluster can. */}}
{{- define "cs.spread" -}}
{{- $root := index . 0 -}}
affinity:
  podAntiAffinity:
    preferredDuringSchedulingIgnoredDuringExecution:
      - weight: 100
        podAffinityTerm:
          topologyKey: kubernetes.io/hostname
          labelSelector:
            matchLabels:
              {{- include "cs.selector" (list $root (index . 1)) | nindent 14 }}
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: topology.kubernetes.io/zone
    whenUnsatisfiable: ScheduleAnyway
    labelSelector:
      matchLabels:
        {{- include "cs.selector" (list $root (index . 1)) | nindent 8 }}
{{- end -}}
