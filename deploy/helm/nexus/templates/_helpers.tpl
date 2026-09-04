{{/*
Expand the name of the chart.
*/}}
{{- define "nexus.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a fully qualified app name.
*/}}
{{- define "nexus.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Chart name and version label value.
*/}}
{{- define "nexus.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels.
*/}}
{{- define "nexus.labels" -}}
helm.sh/chart: {{ include "nexus.chart" . }}
{{ include "nexus.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Selector labels.
*/}}
{{- define "nexus.selectorLabels" -}}
app.kubernetes.io/name: {{ include "nexus.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
ServiceAccount name to use.
*/}}
{{- define "nexus.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "nexus.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Name of the Secret holding sensitive env. Either a user-provided existing
Secret, or one rendered by this chart.
*/}}
{{- define "nexus.secretName" -}}
{{- if .Values.existingSecret -}}
{{- .Values.existingSecret -}}
{{- else -}}
{{- include "nexus.fullname" . -}}
{{- end -}}
{{- end -}}

{{/*
ClickHouse egress rules, shared by the gateway, worker and migration policies.

All three write to ClickHouse — the gateway records traces, the worker records
eval scores, and `nexus migrate --engine=all` applies the ClickHouse
migrations — and until now none of them had a rule for it, so under
mode=enforce the default-deny policy blocked every one. Defined once because
three copies is how the Postgres rules ended up in the worker policy without
the comment that explains them.

Two target modes, mirroring dependencies.postgres: a namespace for a Service in
the cluster, or a CIDR for a managed endpoint outside it.

The in-cluster rule is scoped by namespace and port but not by pod label. The
chart does not know what labels your ClickHouse carries, and guessing one
would render a rule that matches nothing — the same silent block by a longer
route. Postgres can use a podSelector because its selector.matchLabels is an
explicit input; there is no equivalent input here.
*/}}
{{- define "nexus.clickhouseEgress" -}}
{{- if .Values.dependencies.clickhouse.host }}
{{- if .Values.dependencies.clickhouse.namespace }}
# ClickHouse egress — in-cluster Service
- to:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: {{ .Values.dependencies.clickhouse.namespace }}
  ports:
    - protocol: TCP
      port: {{ .Values.dependencies.clickhouse.port }}
{{- end }}
{{- if .Values.dependencies.clickhouse.cidr.enabled }}
# ClickHouse egress — CIDR mode (managed/off-cluster ClickHouse)
- to:
    {{- range $c := .Values.dependencies.clickhouse.cidr.cidrs }}
    - ipBlock:
        cidr: {{ $c }}
    {{- end }}
  ports:
    - protocol: TCP
      port: {{ .Values.dependencies.clickhouse.cidr.port }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
Egress-proxy rule, shared by the gateway and worker policies.

The worker needs this for the same reason the gateway does. External eval
plugins are the worker's job: it forwards traces to Langfuse, LangSmith,
Datadog and the rest over their own auth, and every one of those is
off-cluster. The rule existed only on the gateway, so with enforcement on,
external evals had no route out and failed as vendor timeouts.

Not included in the migration policy — `nexus migrate` talks to the
datastores and nothing else.
*/}}
{{- define "nexus.egressProxy" -}}
{{- if .Values.networkPolicy.egress.proxy.enabled }}
# Egress proxy
- to:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: {{ .Values.networkPolicy.egress.proxy.namespace }}
    {{- if gt (len .Values.networkPolicy.egress.proxy.podSelector) 0 }}
    - podSelector:
        matchLabels:
          {{- range $k, $v := .Values.networkPolicy.egress.proxy.podSelector }}
          {{ $k }}: {{ $v }}
          {{- end }}
    {{- end }}
  ports:
    - protocol: TCP
      port: {{ .Values.networkPolicy.egress.proxy.port }}
{{- end }}
{{- end -}}
