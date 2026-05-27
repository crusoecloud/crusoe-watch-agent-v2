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
Monitoring token secret name.
*/}}
{{- define "cwa.monitoringSecretName" -}}
{{ include "cwa.fullname" . }}-monitoring-token
{{- end }}
