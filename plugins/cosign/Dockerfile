# gocdnext/cosign — sign and verify container images (Sigstore).
#
# Two modes:
#   - keyless (OIDC, default): no long-lived key material. The
#     runner signs using a Sigstore-issued short-lived cert tied
#     to an OIDC token. Requires the job to have an OIDC issuer
#     available — gocdnext's built-in identity or GitHub Actions
#     OIDC both work.
#   - key-based: PLUGIN_KEY points at a cosign private key file
#     written from secrets: PLUGIN_KEY_PASSWORD unlocks it. Use
#     this when keyless can't be wired yet.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_IMAGE          (required)  digest-pinned ref, e.g.
#                                     "ghcr.io/org/app@sha256:abc…".
#                                     cosign STRONGLY prefers
#                                     digest refs — a tag ref
#                                     mutates under you.
#   PLUGIN_ACTION         (optional)  "sign" (default) | "verify" |
#                                     "attest".
#   PLUGIN_KEY            (optional)  path (in workspace) to a
#                                     cosign.key. Empty =
#                                     keyless.
#   PLUGIN_KEY_PASSWORD   (optional)  passphrase for the private
#                                     key, piped from secrets.
#   PLUGIN_CERT_IDENTITY  (optional)  for `verify` keyless: the
#                                     expected OIDC subject
#                                     (e.g. the runner identity
#                                     URL). Required when
#                                     action=verify && no key.
#   PLUGIN_CERT_OIDC_ISSUER (optional) for `verify` keyless: the
#                                     OIDC issuer URL.
#   PLUGIN_REGISTRY       (optional)  login host for push/pull.
#   PLUGIN_USERNAME       (optional)  registry user.
#   PLUGIN_PASSWORD       (optional)  piped from secrets:.
#   PLUGIN_PREDICATE      (optional)  for action=attest: path to
#                                     the in-toto predicate JSON
#                                     (SBOM, SLSA provenance, …).
#   PLUGIN_PREDICATE_TYPE (optional)  for action=attest: e.g.
#                                     "spdxjson", "cyclonedx",
#                                     "slsaprovenance".
FROM gcr.io/projectsigstore/cosign:v2.4.1 AS cosign

FROM alpine:3.20
RUN apk add --no-cache bash ca-certificates
COPY --from=cosign /ko-app/cosign /usr/local/bin/cosign

COPY entrypoint.sh /usr/local/bin/gocdnext-cosign
RUN chmod +x /usr/local/bin/gocdnext-cosign

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-cosign"]
