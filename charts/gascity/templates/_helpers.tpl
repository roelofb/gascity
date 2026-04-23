{{/*
Name helpers. `gascity.fullname` respects .Values.fullnameOverride so the
release-suffixed default never leaks into resource names.
*/}}
{{- define "gascity.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "gascity.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "gascity.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "gascity.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Standard labels shared across every resource. `selectorLabels` is the
immutable subset used in Deployment/StatefulSet selectors.
*/}}
{{- define "gascity.labels" -}}
helm.sh/chart: {{ include "gascity.chart" . }}
{{ include "gascity.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end -}}

{{- define "gascity.selectorLabels" -}}
app.kubernetes.io/name: {{ include "gascity.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "gascity.controllerName" -}}
{{ include "gascity.fullname" . }}
{{- end -}}

{{- define "gascity.doltName" -}}
{{ printf "%s-dolt" (include "gascity.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end -}}

{{- define "gascity.mailName" -}}
{{ printf "%s-mail" (include "gascity.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end -}}

{{/*
Image references. Centralised so the registry prefix only appears once.
*/}}
{{- define "gascity.controllerImage" -}}
{{ printf "%s/%s:%s" .Values.image.registry .Values.image.controller .Values.image.tag }}
{{- end -}}

{{- define "gascity.agentImage" -}}
{{ printf "%s/%s:%s" .Values.image.registry .Values.image.agent .Values.image.tag }}
{{- end -}}

{{- define "gascity.mailImage" -}}
{{ printf "%s/%s:%s" .Values.image.registry .Values.image.mail .Values.image.tag }}
{{- end -}}

{{- define "gascity.doltImage" -}}
{{ printf "%s:%s" .Values.dolt.image .Values.dolt.tag }}
{{- end -}}

{{- define "gascity.imagePullSecrets" -}}
{{- range .Values.image.pullSecrets }}
- name: {{ . }}
{{- end }}
{{- end -}}

{{/*
Hostname for a given service component: "<svc>.<tenant>.<domain>".
Called as (include "gascity.host" (dict "root" . "svc" "discord-admin")).
*/}}
{{- define "gascity.host" -}}
{{- $svc := .svc -}}
{{- $t := .root.Values.tenant -}}
{{- $d := .root.Values.domain -}}
{{- printf "%s.%s.%s" $svc $t $d -}}
{{- end -}}
