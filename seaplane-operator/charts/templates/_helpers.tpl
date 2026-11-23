{{- define "seaplane-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "seaplane-operator.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "seaplane-operator.labels" -}}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "seaplane-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "seaplane-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "seaplane-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "seaplane-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "seaplane-operator.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Build an image reference from registry/repository/tag values. Empty registry
uses shared ECR; a legacy full IMAGE_REPO is split for existing Quay CI overrides.
*/}}
{{- define "seaplane-operator.image" -}}
{{- $registry := .Values.image.registry | default "" -}}
{{- $repository := .Values.image.repository -}}
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
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- $image := printf "%s/%s" $registry $repository -}}
{{- if regexMatch "^sha[0-9]+:" $tag -}}
{{- printf "%s@%s" $image $tag -}}
{{- else -}}
{{- printf "%s:%s" $image $tag -}}
{{- end -}}
{{- end }}
