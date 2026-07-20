{{/*
Expand the name of the chart.
*/}}
{{- define "runs-fleet.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "runs-fleet.fullname" -}}
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

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "runs-fleet.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "runs-fleet.labels" -}}
helm.sh/chart: {{ include "runs-fleet.chart" . }}
{{ include "runs-fleet.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "runs-fleet.selectorLabels" -}}
app.kubernetes.io/name: {{ include "runs-fleet.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Orchestrator labels
*/}}
{{- define "runs-fleet.orchestrator.labels" -}}
{{ include "runs-fleet.labels" . }}
app.kubernetes.io/component: orchestrator
{{- end }}

{{/*
Orchestrator selector labels
*/}}
{{- define "runs-fleet.orchestrator.selectorLabels" -}}
{{ include "runs-fleet.selectorLabels" . }}
app.kubernetes.io/component: orchestrator
{{- end }}

{{/*
Orchestrator service account name
*/}}
{{- define "runs-fleet.orchestrator.serviceAccountName" -}}
{{- if .Values.orchestrator.serviceAccount.name }}
{{- .Values.orchestrator.serviceAccount.name }}
{{- else }}
{{- printf "%s-orchestrator" (include "runs-fleet.fullname" .) }}
{{- end }}
{{- end }}

{{/*
Secret name (GitHub App credentials)
*/}}
{{- define "runs-fleet.secretName" -}}
{{- if .Values.github.existingSecret -}}
{{ .Values.github.existingSecret }}
{{- else -}}
{{ include "runs-fleet.fullname" . }}-secrets
{{- end -}}
{{- end }}

{{/*
Admin secret name (OIDC client secret, session signing secret). Independent
of runs-fleet.secretName -- github.existingSecret and admin.existingSecret
gate two separate Secret objects, so a deployer bringing their own GitHub
Secret isn't forced to also bring their own admin Secret, or vice versa.
*/}}
{{- define "runs-fleet.adminSecretName" -}}
{{- if .Values.admin.existingSecret -}}
{{ .Values.admin.existingSecret }}
{{- else -}}
{{ include "runs-fleet.fullname" . }}-admin-secrets
{{- end -}}
{{- end }}
