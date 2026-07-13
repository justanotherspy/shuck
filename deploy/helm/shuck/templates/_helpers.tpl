{{/*
Chart name, allowing nameOverride.
*/}}
{{- define "shuck.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{/*
Fully qualified release name, allowing fullnameOverride.
*/}}
{{- define "shuck.fullname" -}}
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
{{- end }}

{{/*
Chart label value.
*/}}
{{- define "shuck.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{/*
Effective image tag: image.tag, else the chart appVersion.
*/}}
{{- define "shuck.tag" -}}
{{- .Values.image.tag | default .Chart.AppVersion -}}
{{- end }}

{{/*
Image reference for a backend component.
Context: dict "root" $ "component" "gateway".
*/}}
{{- define "shuck.image" -}}
{{- printf "%s/%s/shuck-%s:%s" .root.Values.image.registry .root.Values.image.owner .component (include "shuck.tag" .root) -}}
{{- end }}

{{/*
Common labels for every resource.
*/}}
{{- define "shuck.labels" -}}
helm.sh/chart: {{ include "shuck.chart" . }}
app.kubernetes.io/name: {{ include "shuck.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ include "shuck.tag" . | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/*
Selector labels for one component.
Context: dict "root" $ "component" "gateway".
*/}}
{{- define "shuck.selectorLabels" -}}
app.kubernetes.io/name: {{ include "shuck.name" .root }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{/*
Name of the Secret every component reads (chart-rendered, operator-provided,
or ESO-produced — same keys either way).
*/}}
{{- define "shuck.secretName" -}}
{{- if .Values.secrets.existingSecret -}}
{{- .Values.secrets.existingSecret -}}
{{- else -}}
{{- include "shuck.fullname" . -}}
{{- end -}}
{{- end }}

{{/*
ServiceAccount name for one component.
Context: dict "root" $ "component" "gateway".
*/}}
{{- define "shuck.serviceAccountName" -}}
{{- $sa := index .root.Values.serviceAccounts .component -}}
{{- if $sa.name -}}
{{- $sa.name -}}
{{- else if $sa.create -}}
{{- printf "%s-%s" (include "shuck.fullname" .root) .component -}}
{{- else -}}
default
{{- end -}}
{{- end }}

{{/*
The deliver URL workers post to: override, else the in-cluster gateway.
*/}}
{{- define "shuck.deliverURL" -}}
{{- if .Values.worker.deliverUrl -}}
{{- .Values.worker.deliverUrl -}}
{{- else -}}
{{- printf "http://%s-gateway:%d/internal/deliver" (include "shuck.fullname" .) (int .Values.gateway.service.port) -}}
{{- end -}}
{{- end }}

{{/*
AWS_REGION env entry (empty when unset — the SDK's default chain applies).
*/}}
{{- define "shuck.awsEnv" -}}
{{- with .Values.aws.region }}
- name: AWS_REGION
  value: {{ . | quote }}
{{- end }}
{{- end }}

{{/*
Portal/sweep validation-mode env: org membership XOR account ownership
(the binary enforces the XOR; the chart just wires whichever is set).
*/}}
{{- define "shuck.portalValidationEnv" -}}
{{- if .Values.github.org }}
- name: SHUCK_GITHUB_ORG
  value: {{ .Values.github.org | quote }}
- name: SHUCK_GITHUB_APP_ID
  value: {{ .Values.github.appId | quote }}
- name: SHUCK_GITHUB_INSTALLATION_ID
  value: {{ .Values.github.installationId | quote }}
- name: SHUCK_GITHUB_APP_PRIVATE_KEY_FILE
  value: /etc/shuck/github-app-private-key.pem
{{- else if .Values.github.accountId }}
- name: SHUCK_GITHUB_ACCOUNT_ID
  value: {{ .Values.github.accountId | quote }}
{{- end }}
{{- end }}

{{/*
GHES base URLs (empty means github.com).
*/}}
{{- define "shuck.ghesEnv" -}}
{{- with .Values.github.webUrl }}
- name: SHUCK_GITHUB_URL
  value: {{ . | quote }}
{{- end }}
{{- with .Values.github.apiUrl }}
- name: SHUCK_GITHUB_API_URL
  value: {{ . | quote }}
{{- end }}
{{- end }}

{{/*
The App private key volume + its standard mount path. Optional so
account-mode portals without an App key still schedule; a component that
needs the key fails fast with the binary's clear "read ..._FILE" error.
*/}}
{{- define "shuck.appKeyVolume" -}}
- name: github-app-key
  secret:
    secretName: {{ include "shuck.secretName" . }}
    optional: true
    items:
      - key: github-app-private-key.pem
        path: github-app-private-key.pem
{{- end }}

{{- define "shuck.appKeyVolumeMount" -}}
- name: github-app-key
  mountPath: /etc/shuck
  readOnly: true
{{- end }}

{{/*
Hardened pod/container security context shared by every component (static
binaries, no privileges needed).
*/}}
{{- define "shuck.podSecurityContext" -}}
runAsNonRoot: true
runAsUser: 65532
runAsGroup: 65532
seccompProfile:
  type: RuntimeDefault
{{- end }}

{{- define "shuck.containerSecurityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities:
  drop: ["ALL"]
{{- end }}

{{/*
Opt-in Prometheus metrics wiring (JUS-96). Empty unless observability.enabled,
so every resident component can include these unconditionally. Context: root.
*/}}
{{- define "shuck.metricsEnv" -}}
{{- if .Values.observability.enabled }}
- name: SHUCK_METRICS_ADDR
  value: ":{{ .Values.observability.port }}"
{{- end }}
{{- end }}

{{- define "shuck.metricsPort" -}}
{{- if .Values.observability.enabled }}
- name: metrics
  containerPort: {{ .Values.observability.port }}
{{- end }}
{{- end }}

{{/*
The `metrics` Service port, added to a component's Service when observability
is enabled so a ServiceMonitor can target it by name. Context: root.
*/}}
{{- define "shuck.metricsServicePort" -}}
{{- if .Values.observability.enabled }}
- name: metrics
  port: {{ .Values.observability.port }}
  targetPort: metrics
{{- end }}
{{- end }}

{{/*
NetworkPolicy ingress rule opening the metrics port to scrapers (empty
observability.networkPolicyFrom means any source). Emitted only when
observability is enabled. Context: root.
*/}}
{{- define "shuck.metricsNetpolIngress" -}}
{{- if .Values.observability.enabled }}
- ports:
    - protocol: TCP
      port: {{ .Values.observability.port }}
  {{- with .Values.observability.networkPolicyFrom }}
  from:
    {{- toYaml . | nindent 4 }}
  {{- end }}
{{- end }}
{{- end }}
