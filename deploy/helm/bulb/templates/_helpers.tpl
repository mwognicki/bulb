{{/*
Full image reference: repository:tag, falling back to .Chart.AppVersion
when image.tag is empty. The release workflow sets appVersion via
`helm package --app-version`, so both the chart version and the image tag
track the same release semver.

This helper is the single source of truth for the image reference. Every
workload template calls it — including the BULB_IMAGE env var in the
controller — so the controller and the proxy DaemonSets it spawns are
always pinned to the same image.
*/}}
{{- define "bulb.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end }}

{{/*
Common labels applied to every resource.
*/}}
{{- define "bulb.labels" -}}
app.kubernetes.io/name: bulb
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels used in matchLabels + pod template labels.
Accepts a dict with keys "component" and "root" (the chart root context).
Usage: {{ include "bulb.selectorLabels" (dict "component" "controller" "root" .) }}
*/}}
{{- define "bulb.selectorLabels" -}}
app.kubernetes.io/name: bulb
app.kubernetes.io/component: {{ .component }}
{{- end }}
