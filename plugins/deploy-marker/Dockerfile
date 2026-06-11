# gocdnext/deploy-marker — record a deploy event in the
# observability stack (Datadog events API or Grafana annotations).
#
# Contract:
#   PLUGIN_PROVIDER    (required)  datadog | grafana.
#   PLUGIN_TEXT        (optional)  event body; sane CI default.
#   PLUGIN_TAGS        (optional)  comma-separated key:value tags.
#   DATADOG_API_KEY                via `secrets:` (datadog).
#   GRAFANA_TOKEN + PLUGIN_GRAFANA_URL  via `secrets:` (grafana).
#   PLUGIN_API_BASE    (optional)  override for proxies/testing.
FROM alpine:3.21

RUN apk add --no-cache curl jq

COPY entrypoint.sh /usr/local/bin/gocdnext-deploy-marker
RUN chmod +x /usr/local/bin/gocdnext-deploy-marker

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-deploy-marker"]
