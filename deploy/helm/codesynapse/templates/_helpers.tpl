{{- define "cs.image" -}}
{{- $root := index . 0 -}}
{{- printf "%s/codesynapse-%s:%s" $root.Values.image.registry (index . 1) ($root.Values.image.tag | default $root.Chart.AppVersion) -}}
{{- end -}}






{{/* Store addresses: in-chart service or the external value. */}}
{{- define "cs.nebulaAddrs" -}}
{{- if .Values.nebula.enabled }}{{ include "cs.name" (list . "nebula") }}:9669{{ else }}{{ required "nebula.externalAddrs is required when nebula.enabled is false" .Values.nebula.externalAddrs }}{{ end -}}
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
{{- define "cs.logsURL" -}}
{{- if .Values.victorialogs.enabled }}http://{{ include "cs.name" (list . "victorialogs") }}:9428{{ else }}{{ .Values.victorialogs.externalURL }}{{ end -}}
{{- end -}}

{{/* The vmalert the API reads alerts from: the chart's, or alerting.externalURL. */}}
{{- define "cs.alertsURL" -}}
{{- if .Values.alerting.enabled }}http://{{ include "cs.name" (list . "vmalert") }}:8880{{ else }}{{ .Values.alerting.externalURL }}{{ end -}}
{{- end -}}

{{/* Environment shared by api, worker and mcp: stores and telemetry. */}}
{{- define "cs.storeEnv" -}}
- {name: CODESYNAPSE_NEBULA_ADDRS, value: {{ include "cs.nebulaAddrs" . | quote }}}
- {name: CODESYNAPSE_NEBULA_USER, value: root}
{{- if and .Values.nebula.enabled (gt (int .Values.nebula.space.replicas) 1) }}
{{- fail "nebula.space.replicas above 1 needs a NebulaGraph cluster: the in-chart one has a single storaged host" }}
{{- end }}
- {name: CODESYNAPSE_NEBULA_PARTITIONS, value: {{ .Values.nebula.space.partitions | quote }}}
- {name: CODESYNAPSE_NEBULA_REPLICAS, value: {{ .Values.nebula.space.replicas | quote }}}
{{ include "cs.secretEnv" (list . "CODESYNAPSE_NEBULA_PASSWORD" "NEBULA_PASSWORD") }}
- {name: CODESYNAPSE_MILVUS_URL, value: {{ include "cs.milvusURL" . | quote }}}
{{ include "cs.secretEnv" (list . "CODESYNAPSE_MILVUS_TOKEN" "MILVUS_TOKEN" true) }}
- {name: CODESYNAPSE_METRICS_URL, value: {{ include "cs.metricsURL" . | quote }}}
- {name: CODESYNAPSE_METRICS_USER, value: codesynapse}
{{ include "cs.secretEnv" (list . "CODESYNAPSE_METRICS_PASSWORD" "METRICS_PASSWORD") }}
- {name: CODESYNAPSE_TRACES_URL, value: {{ include "cs.tracesURL" . | quote }}}
- {name: CODESYNAPSE_TRACES_USER, value: codesynapse}
{{ include "cs.secretEnv" (list . "CODESYNAPSE_TRACES_PASSWORD" "METRICS_PASSWORD") }}
- {name: CODESYNAPSE_TRACES_SAMPLE, value: {{ .Values.traces.sample | quote }}}
- {name: CODESYNAPSE_LOGS_URL, value: {{ include "cs.logsURL" . | quote }}}
- {name: CODESYNAPSE_LOGS_USER, value: codesynapse}
{{ include "cs.secretEnv" (list . "CODESYNAPSE_LOGS_PASSWORD" "METRICS_PASSWORD") }}
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
{{- if eq (toString (index . 1)) "true" }}
{{ include "cs.secretEnv" (list $root "CODESYNAPSE_GIT_PUSH_TOKENS" "CODESYNAPSE_GIT_PUSH_TOKENS" true) }}
{{- end }}
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
{{- with .Values.auditArchive }}
- {name: CODESYNAPSE_AUDIT_HOT_DAYS, value: {{ .hotDays | quote }}}
{{- if .s3.endpoint }}
{{- if not .s3.bucket }}{{ fail "auditArchive.s3.bucket is required with an endpoint" }}{{ end }}
- {name: CODESYNAPSE_AUDIT_S3_ENDPOINT, value: {{ .s3.endpoint | quote }}}
- {name: CODESYNAPSE_AUDIT_S3_BUCKET, value: {{ .s3.bucket | quote }}}
- {name: CODESYNAPSE_AUDIT_S3_REGION, value: {{ .s3.region | quote }}}
- {name: CODESYNAPSE_AUDIT_S3_PREFIX, value: {{ .s3.prefix | quote }}}
- {name: CODESYNAPSE_AUDIT_S3_LOCK_MODE, value: {{ .s3.lockMode | quote }}}
{{ include "cs.secretEnv" (list $ "CODESYNAPSE_AUDIT_S3_ACCESS_KEY_ID" "CODESYNAPSE_AUDIT_S3_ACCESS_KEY_ID") }}
{{ include "cs.secretEnv" (list $ "CODESYNAPSE_AUDIT_S3_SECRET_ACCESS_KEY" "CODESYNAPSE_AUDIT_S3_SECRET_ACCESS_KEY") }}
{{- end }}
{{- end }}
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
