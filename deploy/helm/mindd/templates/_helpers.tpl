{{/* vim: set filetype=mustache: */}}

{{/* Common name helpers */}}
{{- define "mindd.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "mindd.fullname" -}}
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

{{- define "mindd.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "mindd.labels" -}}
helm.sh/chart: {{ include "mindd.chart" . }}
app.kubernetes.io/name: {{ include "mindd.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "mindd.selectorLabels" -}}
app.kubernetes.io/name: {{ include "mindd.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "mindd.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "mindd.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "mindd.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}

{{- define "mindd.tlsSecretName" -}}
{{- if .Values.tls.existingSecret -}}
{{- .Values.tls.existingSecret -}}
{{- else -}}
{{- printf "%s-tls" (include "mindd.fullname" .) -}}
{{- end -}}
{{- end -}}
