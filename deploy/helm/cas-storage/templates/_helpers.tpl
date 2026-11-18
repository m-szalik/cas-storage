{{- define "cas-storage.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "cas-storage.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else if contains (include "cas-storage.name" .) .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name (include "cas-storage.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{- define "cas-storage.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cas-storage.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "cas-storage.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{ include "cas-storage.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "cas-storage.secretName" -}}
{{- .Values.existingSecret | default (printf "%s-config" (include "cas-storage.fullname" .)) }}
{{- end }}

{{- define "cas-storage.claimName" -}}
{{- .Values.persistence.existingClaim | default (printf "%s-data" (include "cas-storage.fullname" .)) }}
{{- end }}
