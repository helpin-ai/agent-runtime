{{- define "agent-runtime.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "agent-runtime.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "agent-runtime.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

