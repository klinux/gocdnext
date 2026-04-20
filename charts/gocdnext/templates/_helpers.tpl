{{/*
Expand the name of the chart.
*/}}
{{- define "gocdnext.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully-qualified release-scoped name. Used to stamp every object so
parallel releases in the same namespace don't collide.
*/}}
{{- define "gocdnext.fullname" -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "gocdnext.server.fullname" -}}
{{- printf "%s-server" (include "gocdnext.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "gocdnext.agent.fullname" -}}
{{- printf "%s-agent" (include "gocdnext.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "gocdnext.web.fullname" -}}
{{- printf "%s-web" (include "gocdnext.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "gocdnext.postgres.fullname" -}}
{{- printf "%s-postgres" (include "gocdnext.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Standard labels on every object. Recommended Kubernetes labels.
*/}}
{{- define "gocdnext.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
app.kubernetes.io/name: {{ include "gocdnext.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "gocdnext.server.selectorLabels" -}}
app.kubernetes.io/name: {{ include "gocdnext.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: server
{{- end -}}

{{- define "gocdnext.agent.selectorLabels" -}}
app.kubernetes.io/name: {{ include "gocdnext.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: agent
{{- end -}}

{{- define "gocdnext.web.selectorLabels" -}}
app.kubernetes.io/name: {{ include "gocdnext.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: web
{{- end -}}

{{/*
Image string: registry/repo:tag. Tag defaults to Chart.AppVersion
so helm upgrade bumps images automatically on appVersion changes.
*/}}
{{- define "gocdnext.image" -}}
{{- $registry := .root.Values.global.imageRegistry | default "" -}}
{{- $repo := .imageRef.repository -}}
{{- $tag := .imageRef.tag | default .root.Chart.AppVersion -}}
{{- if $registry -}}
{{- printf "%s/%s:%s" $registry $repo $tag -}}
{{- else -}}
{{- printf "%s:%s" $repo $tag -}}
{{- end -}}
{{- end -}}

{{/*
Database URL source — either inline or existingSecret reference.
Returns a single env entry rendered YAML.
*/}}
{{- define "gocdnext.database.envVar" -}}
- name: GOCDNEXT_DATABASE_URL
{{- if .Values.database.existingSecret }}
  valueFrom:
    secretKeyRef:
      name: {{ .Values.database.existingSecret }}
      key: {{ .Values.database.secretKey }}
{{- else if .Values.devDatabase.enabled }}
  value: {{ printf "postgres://%s:%s@%s:5432/%s?sslmode=disable" .Values.devDatabase.credentials.user .Values.devDatabase.credentials.password (include "gocdnext.postgres.fullname" .) .Values.devDatabase.credentials.database | quote }}
{{- else }}
  value: {{ required "database.url is required when devDatabase.enabled=false and database.existingSecret is empty" .Values.database.url | quote }}
{{- end -}}
{{- end -}}
