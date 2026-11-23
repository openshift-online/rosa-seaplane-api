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

{{/*
Build an image reference from registry/repository/tag values. Empty registry
uses the shared ECR account. A legacy full IMAGE_REPO value is split so the
existing Quay-based component pre-merge override continues to work.
*/}}
{{- define "platform-api.image" -}}
{{- $registry := .Values.platformApi.app.image.registry | default "" -}}
{{- $repository := .Values.platformApi.app.image.repository -}}
{{- if and (not $registry) (contains "/" $repository) -}}
{{- $parts := splitList "/" $repository -}}
{{- $host := first $parts -}}
{{- if or (contains "." $host) (contains ":" $host) (eq $host "localhost") -}}
{{- $registry = $host -}}
{{- $repository = join "/" (rest $parts) -}}
{{- end -}}
{{- end -}}
{{- if not $registry -}}
{{- $registry = printf "%s.dkr.ecr.us-east-1.amazonaws.com" .Values.global.image_registry_account_id -}}
{{- end -}}
{{- $image := printf "%s/%s" $registry $repository -}}
{{- $tag := .Values.platformApi.app.image.tag -}}
{{- if regexMatch "^sha[0-9]+:" $tag -}}
{{- printf "%s@%s" $image $tag -}}
{{- else -}}
{{- printf "%s:%s" $image $tag -}}
{{- end -}}
{{- end -}}
