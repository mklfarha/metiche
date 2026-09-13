{{- define "metiche-mysql.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "metiche-mysql.fullname" -}}
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

{{- define "metiche-mysql.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "metiche-mysql.labels" -}}
helm.sh/chart: {{ include "metiche-mysql.chart" . }}
{{ include "metiche-mysql.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: metiche
app.kubernetes.io/component: database
{{- end -}}

{{- define "metiche-mysql.selectorLabels" -}}
app.kubernetes.io/name: {{ include "metiche-mysql.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "metiche-mysql.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "metiche-mysql.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
The probe command. The password is read from the mounted Secret file by the
probe's own shell, so it never appears in argv — `ps` inside the container,
/proc/<pid>/cmdline, and the kubelet's log of a failed probe would all carry
it otherwise.
*/}}
{{- define "metiche-mysql.pingCommand" -}}
- sh
- -c
- 'export MYSQL_PWD="$(cat {{ .Values.probes.secretMountPath }}/root-password)"; exec mysqladmin ping -h 127.0.0.1 -u root --silent'
{{- end -}}

{{/*
The mysql image tag, which must be a string.

Unlike the app images, a numeric mysql tag cannot be repaired: `tag: 8.0`
unquoted is the number 8, and "mysql:8" is precisely the unpinned tag
values.yaml forbids. Refuse the render instead of guessing.
*/}}
{{- define "metiche-mysql.imageTag" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- if not (kindIs "string" $tag) -}}
{{- fail (printf "image.tag must be a quoted string, got the %s %v. Quote it in values or pass --set-string image.tag=..." (kindOf $tag) $tag) -}}
{{- end -}}
{{- $tag -}}
{{- end -}}
