{{/*
Chart name.
*/}}
{{- define "cwa.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name.
*/}}
{{- define "cwa.fullname" -}}
{{- default .Chart.Name .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "cwa.labels" -}}
app.kubernetes.io/name: {{ include "cwa.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Labels for the RBAC objects in rbac.yaml. Deliberately omits app.kubernetes.io/version
so their rendered content never changes between chart versions.
*/}}
{{- define "cwa.bootstrapLabels" -}}
app.kubernetes.io/name: {{ include "cwa.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "cwa.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cwa.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "cwa.serviceAccountName" -}}
{{ include "cwa.fullname" . }}
{{- end }}

{{/*
Monitoring token secret name. Fixed, not chart-name derived: credentials.cleanup
deletes this Secret by a name compiled into the agent.
*/}}
{{- define "cwa.monitoringSecretName" -}}
crusoe-monitoring-token
{{- end }}

{{/*
Prepend global.registry (default ghcr.io) to a host-less repository. A
repository whose first segment has "." or ":" already has a host and is kept.
Args: ctx, repository.
*/}}
{{- define "cwa.repository" -}}
{{- $first := splitList "/" .repository | first -}}
{{- if or (contains "." $first) (contains ":" $first) -}}
  {{- .repository -}}
{{- else -}}
  {{- $registry := (.ctx.Values.global | default dict).registry | default "ghcr.io" | trimSuffix "/" -}}
  {{- printf "%s/%s" $registry .repository -}}
{{- end -}}
{{- end -}}

{{/* Build a full image reference. Args: ctx, repository, tag. */}}
{{- define "cwa.image" -}}
{{- $repo := include "cwa.repository" (dict "ctx" .ctx "repository" .repository) -}}
{{- if .tag -}}
  {{- printf "%s:%s" $repo (.tag | toString) -}}
{{- else -}}
  {{- $repo -}}
{{- end -}}
{{- end -}}
