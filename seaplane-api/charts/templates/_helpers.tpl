{{/*
Expand the name of the chart.
*/}}
{{- define "platform-api.name" -}}
{{- default .Chart.Name .Values.platformApi.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "platform-api.fullname" -}}
{{- if .Values.platformApi.fullnameOverride }}
{{- .Values.platformApi.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.platformApi.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "platform-api.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "platform-api.labels" -}}
helm.sh/chart: {{ include "platform-api.chart" . }}
{{ include "platform-api.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "platform-api.selectorLabels" -}}
app.kubernetes.io/name: {{ include "platform-api.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
