{{/* Chart name, overridable by fullnameOverride/nameOverride. */}}
{{- define "palai.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "palai.fullname" -}}
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

{{- define "palai.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Common labels. */}}
{{- define "palai.labels" -}}
helm.sh/chart: {{ include "palai.chart" . }}
{{ include "palai.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/* Selector labels — the stable identity the Deployment/Service/NetworkPolicy match on. */}}
{{- define "palai.selectorLabels" -}}
app.kubernetes.io/name: {{ include "palai.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "palai.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "palai.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
palai.controlPlaneEnv — the shared, NON-SECRET environment both the Deployment and the migration
Job set. Secret values (DB URL, S3 creds) are injected via valueFrom.secretKeyRef at their use site,
never here, so they never land in a rendered non-secret object. Fails the render early if a required
external reference is missing, so a broken install is caught at `helm template`, not at runtime.
*/}}
{{- define "palai.databaseEnv" -}}
{{- if not .Values.postgres.existingSecret -}}
{{- fail "postgres.existingSecret is required (the DB URL must ride a Secret, never values)" -}}
{{- end -}}
- name: PALAI_DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.postgres.existingSecret | quote }}
      key: {{ .Values.postgres.urlKey | quote }}
{{- end -}}
