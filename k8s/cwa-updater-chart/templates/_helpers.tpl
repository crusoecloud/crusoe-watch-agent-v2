{{/*
Chart name.
*/}}
{{- define "cwaUpdater.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name. Kept equal to the chart name: cwa-manager resolves
cwa-updater by DNS at a fixed address (cwa-updater.<ns>.svc.cluster.local:8786),
so the Service name must not vary with the release name.
*/}}
{{- define "cwaUpdater.fullname" -}}
{{- default .Chart.Name .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "cwaUpdater.labels" -}}
app.kubernetes.io/name: {{ include "cwaUpdater.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "cwaUpdater.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cwaUpdater.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "cwaUpdater.serviceAccountName" -}}
{{ include "cwaUpdater.fullname" . }}
{{- end }}
