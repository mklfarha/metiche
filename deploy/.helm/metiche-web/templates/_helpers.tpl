{{- define "metiche-web.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "metiche-web.fullname" -}}
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

{{- define "metiche-web.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "metiche-web.labels" -}}
helm.sh/chart: {{ include "metiche-web.chart" . }}
{{ include "metiche-web.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: metiche
app.kubernetes.io/component: board
{{- end -}}

{{- define "metiche-web.selectorLabels" -}}
app.kubernetes.io/name: {{ include "metiche-web.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "metiche-web.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "metiche-web.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
The backend base URL: an explicit value if given, otherwise derived from the
backend Service in this namespace. Empty when neither is set, which is the
signal to run in fixture-replay mode.

Derived rather than written down: the hostname must not be a constant in a
template any more than in the image. Cluster-internal DNS, so the board's hop
to the backend never leaves the node and never touches the public ingress.
*/}}
{{- define "metiche-web.backendURL" -}}
{{- if .Values.backend.url -}}
{{- .Values.backend.url -}}
{{- else if .Values.backend.serviceName -}}
{{- printf "http://%s.%s.svc.cluster.local:%v" .Values.backend.serviceName .Release.Namespace .Values.backend.port -}}
{{- end -}}
{{- end -}}

{{/*
SSE ingress annotations. The board's /t/{slug}/stream is the stream a person
actually looks at; see values.yaml `sse:` for why each of these is required.
*/}}
{{- define "metiche-web.sseAnnotations" -}}
{{- if .Values.sse.enabled -}}
# SSE: the board holds /t/{slug}/stream open for the life of the tab.
# nginx buffers proxied responses by default, which means the board paints
# nothing at all until a buffer fills. Off.
nginx.ingress.kubernetes.io/proxy-buffering: "off"
{{- if .Values.sse.disableRequestBuffering }}
nginx.ingress.kubernetes.io/proxy-request-buffering: "off"
{{- end }}
# SSE: a quiet team streams nothing for minutes and that is normal. nginx's
# 60s idle default would cut the stream; htmx reconnects, so the symptom is a
# board that flickers every minute and occasionally drops a frame — and it
# looks exactly like a frontend bug.
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
{{- define "metiche-web.imageTag" -}}
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
