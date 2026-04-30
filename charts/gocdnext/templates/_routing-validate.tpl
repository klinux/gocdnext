{{- /*
Mutual-exclusion guard for the routing modes. Operators can pick:
  - per-component Ingress (server.ingress + web.ingress)
  - unified Ingress (top-level `ingress`)
  - per-component Gateway (server.gateway + web.gateway)
  - unified Gateway (top-level `gateway`)
Mixing per-component AND unified at the same layer (Ingress or
Gateway) creates two routes for the same host and the cluster's
controller decides arbitrarily which wins. Catch it at template
time so the operator picks one explicitly.

Mixing Ingress AND Gateway is allowed — useful during migrations.
*/}}
{{- define "gocdnext.validate.routing" -}}
{{- if and .Values.ingress.enabled (or .Values.server.ingress.enabled .Values.web.ingress.enabled) -}}
{{- fail "ingress.enabled=true is mutually exclusive with server.ingress.enabled / web.ingress.enabled. Pick one routing model: unified single-host (top-level `ingress`) OR per-component (server.ingress + web.ingress)." -}}
{{- end -}}
{{- if and .Values.gateway.enabled (or .Values.server.gateway.enabled .Values.web.gateway.enabled) -}}
{{- fail "gateway.enabled=true is mutually exclusive with server.gateway.enabled / web.gateway.enabled." -}}
{{- end -}}
{{- end -}}
