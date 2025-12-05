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
Runner labels
*/}}
{{- define "runs-fleet.runner.labels" -}}
{{ include "runs-fleet.labels" . }}
app.kubernetes.io/component: runner
{{- end }}

{{/*
Valkey labels
*/}}
{{- define "runs-fleet.valkey.labels" -}}
{{ include "runs-fleet.labels" . }}
app.kubernetes.io/component: valkey
{{- end }}

{{/*
Valkey selector labels
*/}}
{{- define "runs-fleet.valkey.selectorLabels" -}}
{{ include "runs-fleet.selectorLabels" . }}
app.kubernetes.io/component: valkey
{{- end }}

{{/*
Orchestrator service account name
*/}}
{{- define "runs-fleet.orchestrator.serviceAccountName" -}}
{{ include "runs-fleet.fullname" . }}-orchestrator
{{- end }}

{{/*
Runner service account name
*/}}
{{- define "runs-fleet.runner.serviceAccountName" -}}
{{- default (printf "%s-runner" (include "runs-fleet.fullname" .)) .Values.runner.serviceAccountName }}
{{- end }}

{{/*
Valkey address
*/}}
{{- define "runs-fleet.valkey.address" -}}
{{ include "runs-fleet.fullname" . }}-valkey.{{ .Release.Namespace }}.svc.cluster.local:6379
{{- end }}

{{/*
Secret name
*/}}
{{- define "runs-fleet.secretName" -}}
{{- if .Values.github.existingSecret -}}
{{ .Values.github.existingSecret }}
{{- else -}}
{{ include "runs-fleet.fullname" . }}-secrets
{{- end -}}
{{- end }}

{{/*
Node selector as string (key=value,key=value format)
*/}}
{{- define "runs-fleet.nodeSelector.string" -}}
{{- $selectors := list -}}
{{- range $key, $value := .Values.runner.nodeSelector -}}
{{- $selectors = append $selectors (printf "%s=%s" $key $value) -}}
{{- end -}}
{{- join "," $selectors -}}
{{- end }}

{{/*
Validate mode selection
*/}}
{{- define "runs-fleet.validateMode" -}}
{{- if and (ne .Values.mode "ec2") (ne .Values.mode "k8s") -}}
{{- fail "mode must be 'ec2' or 'k8s'" -}}
{{- end -}}
{{- end }}
