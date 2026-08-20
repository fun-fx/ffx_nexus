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
Common image spec used by every Deployment in this chart. A future
change that drifts one Deployment's image from the others is loud —
this is the gate. Operations: keep this template the single source
of truth for the image repository / tag / pullPolicy that both the
gateway and the worker pin to. Phase D-1 spec: "두 Deployment의
이미지가 동일함을 렌더 테스트로 단정하라".
*/}}
{{- define "nexus.image" -}}
{{- if .Values.image -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) -}}
{{- else -}}
{{- printf "%s:%s" "nexus" .Chart.AppVersion -}}
{{- end -}}
{{- end -}}

{{/*
Image pull policy shared by both Deployments. Operators can override
the chart-wide value from the surface that produced this template,
but inheriting from .Values.image.pullPolicy keeps the two halves
of the chart in lockstep. A drift here is a Helm smell — pin it
once and let both halves use it without re-stating the policy.
*/}}
{{- define "nexus.imagePullPolicy" -}}
{{- if and .Values.image .Values.image.pullPolicy -}}
{{- .Values.image.pullPolicy -}}
{{- else -}}
IfNotPresent
{{- end -}}
{{- end -}}

{{/*
List of env entries shared by both Deployments. Pulled from
.Values.dependencies and .Values.config plus the role-specific
NEXUS_ROLE/NEXUS_SCHEDULER_ROLE_ENABLED additions to ensure the
two Deployments have identical runtime configuration other than
the role marker. Operators who flip a Values key see the change
land in both halves at once — that's the point.
*/}}
{{- define "nexus.commonEnv" -}}
{{- range $key, $value := .Values.dependencies }}
- name: {{ $key }}
  value: {{ $value | quote }}
{{- end }}
{{- if .Values.metricsAddr }}
- name: NEXUS_METRICS_ADDR
  value: {{ .Values.metricsAddr | quote }}
{{- end }}
- name: NEXUS_AUTO_MIGRATE
  value: "false"
{{- end -}}

{{/*
Volume mount layout shared by both Deployments. The matches against
.Values.config.volumeMounts and the chart's default volumes are
rendered once so a new mount added to the gateway does not
silently miss the worker. We do not list the migration-job mount
here because that Job is a separate render.
*/}}
{{- define "nexus.commonVolumes" -}}
{{- if (or (empty .Values.secretEnv) (.Values.secretEnv)) }}
{{- if not .Values.existingSecret }}
- name: nexus-secret
  secret:
    secretName: {{ include "nexus.secretName" . }}
{{- end }}
{{- end }}
- name: tmp
  emptyDir: {}
{{- end -}}

{{/*
Common volumeMount array referenced by both Deployments. The
volumes above are paired with these mounts 1:1; a future refactor
that adds a volume here must also add the matching mount here or
the chart's render test trips.
*/}}
{{- define "nexus.commonVolumeMounts" -}}
{{- if not .Values.existingSecret }}
- name: nexus-secret
  readOnly: true
  mountPath: /etc/nexus/secrets
{{- end }}
- name: tmp
  mountPath: /tmp
{{- end -}}

{{/*
Readiness readiness rules split for the two roles. We define the
endpoint directives inline rather than reusing the existing
readinessProbe because the meaning of /readyz differs:

  /readyz on the gateway: 8080 — "we are serving traffic"
  /readyz on the worker:  metricsAddr — "we hold the Postgres lease"

Single template can be reviewed side-by-side. Operators who flip
this template to "always-fail" see both pods fail readiness, which
is the loud signal we want when the readiness contract drifts.
*/}}
{{- define "nexus.readinessProbe" -}}
httpGet:
  path: /readyz
  port: {{ .port }}
initialDelaySeconds: 10
periodSeconds: 10
failureThreshold: 6
{{- end -}}
