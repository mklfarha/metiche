{{/*
Everything identity-bearing is a literal here, not derived from the release
name: the StatefulSet name is the pod hostname, which is an input to the
keyring passphrase (docs/NUZUR_AGENT.md §4.D), and the pod labels are what the
metiche-mysql NetworkPolicy admits. A second release, or a renamed one, must
fail to render rather than quietly become a different identity.
*/}}
{{- define "nuzur-agent.guard" -}}
{{- if ne .Release.Name "nuzur-agent" -}}
{{- fail (printf "install this chart as release \"nuzur-agent\" (got %q): the StatefulSet name is the pod hostname, part of the agent's keyring passphrase, and the MySQL NetworkPolicy admits the pod by the nuzur-agent labels." .Release.Name) -}}
{{- end -}}
{{- end -}}

{{- define "nuzur-agent.mode" -}}
{{- $mode := .Values.agent.mode -}}
{{- if not (kindIs "string" $mode) -}}
{{- fail (printf "agent.mode must be the string \"setup\" or \"run\", got the %s %v" (kindOf $mode) $mode) -}}
{{- end -}}
{{- if not (has $mode (list "setup" "run")) -}}
{{- fail (printf "agent.mode is required and must be \"setup\" or \"run\" (got %q). Pass --set-string agent.mode=setup for the one-time exec, or =run once setup is complete. See deploy/scripts/nuzur-agent-setup.md." $mode) -}}
{{- end -}}
{{- $mode -}}
{{- end -}}

{{/*
The image tag as the exact string that was built, e.g. 1.9.2-bfe9be0. Like the
metiche-mysql chart, a non-string tag is refused rather than repaired: pass
--set-string image.tag=...
*/}}
{{- define "nuzur-agent.imageTag" -}}
{{- $tag := .Values.image.tag -}}
{{- if not (kindIs "string" $tag) -}}
{{- fail (printf "image.tag must be a quoted string, got the %s %v. Pass --set-string image.tag=..." (kindOf $tag) $tag) -}}
{{- end -}}
{{- if eq $tag "" -}}
{{- fail "image.tag is required: the tag nuzur-agent-setup.sh image printed. imagePullPolicy is Never, so it must already be in containerd." -}}
{{- end -}}
{{- $tag -}}
{{- end -}}

{{- define "nuzur-agent.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "nuzur-agent.selectorLabels" -}}
app.kubernetes.io/name: nuzur-agent
app.kubernetes.io/instance: nuzur-agent
{{- end -}}

{{- define "nuzur-agent.labels" -}}
helm.sh/chart: {{ include "nuzur-agent.chart" . }}
{{ include "nuzur-agent.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: metiche
app.kubernetes.io/component: nuzur-agent
{{- end -}}

{{/*
The whole environment of both containers. Nothing else is ever set: in
particular no NUZUR_* variable (NUZUR_AGENT_DSN / NUZUR_AGENT_DRIVER would
serve a plaintext fallback DSN, §4.E), and the preflight refuses to start if
any appears. USER is part of the keyring passphrase; XDG_CONFIG_HOME must be
absolute and on the PVC or the CLI falls back to /tmp (§4.C).
*/}}
{{- define "nuzur-agent.env" -}}
- name: HOME
  value: /var/lib/nuzur
- name: XDG_CONFIG_HOME
  value: /var/lib/nuzur/config
- name: USER
  value: nuzur
- name: LOGNAME
  value: nuzur
{{- end -}}

{{- define "nuzur-agent.containerSecurityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
runAsNonRoot: true
runAsUser: 10001
runAsGroup: 10001
capabilities:
  drop: ["ALL"]
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{- define "nuzur-agent.machineIdMount" -}}
- name: machine-id
  mountPath: /etc/machine-id
  subPath: machine-id
  readOnly: true
{{- end -}}
