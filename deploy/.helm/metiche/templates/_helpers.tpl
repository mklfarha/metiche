{{/*
Chart name, overridable.
*/}}
{{- define "metiche.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified release name. The api/mcp split sets fullnameOverride so two
releases in one namespace do not both try to own "metiche".
*/}}
{{- define "metiche.fullname" -}}
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

{{- define "metiche.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "metiche.labels" -}}
helm.sh/chart: {{ include "metiche.chart" . }}
{{ include "metiche.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: metiche
metiche.xyz/role: {{ .Values.role | quote }}
{{- end -}}

{{- define "metiche.selectorLabels" -}}
app.kubernetes.io/name: {{ include "metiche.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "metiche.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "metiche.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
The CONFIG value handed to the generated config loader
(code/backend/metiche/config/config.go). It is a comma-separated list and
later entries win, so this is: the image's committed placeholders, then the
real overlay mounted from a Secret.
*/}}
{{- define "metiche.configPath" -}}
{{- printf "%s,%s/%s" .Values.config.imageConfigDir (.Values.config.mountPath | trimSuffix "/") .Values.config.key -}}
{{- end -}}

{{/*
The SSE-critical ingress annotations. See values.yaml `sse:` for why each one
exists; the short version is that without them the board's stream dies at
nginx's 60s idle timeout and the failure looks like a frontend bug.
*/}}
{{- define "metiche.sseAnnotations" -}}
{{- if .Values.sse.enabled -}}
# SSE: nginx buffers proxied responses by default, which holds an
# event stream until the buffer fills. Off, or the board shows nothing.
nginx.ingress.kubernetes.io/proxy-buffering: "off"
{{- if .Values.sse.disableRequestBuffering }}
nginx.ingress.kubernetes.io/proxy-request-buffering: "off"
{{- end }}
# SSE: an idle stream is normal (a quiet team sends nothing for minutes).
# nginx's 60s default would cut it and the browser would reconnect in a loop.
nginx.ingress.kubernetes.io/proxy-read-timeout: {{ .Values.sse.readTimeout | quote }}
nginx.ingress.kubernetes.io/proxy-send-timeout: {{ .Values.sse.sendTimeout | quote }}
{{- end }}
{{- end -}}
