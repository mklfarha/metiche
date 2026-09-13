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

{{/*
The image tag as the exact string that was built.

Tags are timestamps (20260912213026). Helm types an unquoted number as one, and
a release upgraded with --reuse-values round-trips its stored values through
JSON, so the tag comes back as float64 2.0260912213026e+13 — which renders into
the image reference as-is and fails as InvalidImageName. toString, quote and
printf "%v" all print that float the same broken way.

An integral float below 2^53 still holds the exact digits, so it is printed as
an integer. Anything else numeric cannot be recovered and fails the render:
pass tags with --set-string (deploy/scripts/helm-deploy.sh does).
*/}}
{{- define "metiche.imageTag" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- if or (kindIs "float64" $tag) (kindIs "float32" $tag) -}}
{{- if or (ge (float64 $tag) 9007199254740992.0) (lt (float64 $tag) 0.0) (ne (float64 (int64 $tag)) (float64 $tag)) -}}
{{- fail (printf "image.tag was parsed as the number %v and its original spelling is lost. Pass it with --set-string image.tag=... and do not use --reuse-values." $tag) -}}
{{- end -}}
{{- printf "%d" (int64 $tag) -}}
{{- else if kindIs "string" $tag -}}
{{- $tag -}}
{{- else -}}
{{- printf "%d" (int64 $tag) -}}
{{- end -}}
{{- end -}}
