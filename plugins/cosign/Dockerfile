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
# cosign binary bumped v2.4.1 -> v3.1.4 (pinned by digest). v2.4.1 was built on an
# old Go and bundled vulnerable deps (Go-stdlib CVE-2025-68121, grpc
# CVE-2026-33186); v3.1.4 is built with Go 1.27.1 and Trivy on its binary is 0
# CRITICAL. The v2->v3 bump is a major cosign release and MAY be breaking (bundle
# format / defaults) for consumers — but the plugin's surface is unchanged: sign /
# verify / attest / login and the flags it passes (--key, --certificate-identity,
# --certificate-oidc-issuer, --predicate, --type, --yes; COSIGN_PASSWORD via env)
# all exist in v3.1.4 (proven by the smoke).
FROM gcr.io/projectsigstore/cosign:v3.1.4@sha256:0f06154836d2de34f887a5c273506d52ca43edaa1d9e53e6510fa78a48f69c2d AS cosign

# Base: a SUPPORTED Alpine (3.24, pinned by digest) + apk upgrade. alpine:3.20
# went EOL 2026-04-01 (no more security updates / degraded scanner coverage).
# NOTE: `apk upgrade` pulls Alpine's MUTABLE repos — the FROM digest pins the base
# layer, not the upgraded set; the weekly Trivy gate catches drift.
FROM alpine:3.24.4@sha256:93b41ef40b1677fce8fcc64ff4af09daddfacf5040afde618bc46c1e2d1cc7d4
RUN apk upgrade --no-cache && apk add --no-cache bash ca-certificates
COPY --from=cosign /ko-app/cosign /usr/local/bin/cosign

COPY entrypoint.sh /usr/local/bin/gocdnext-cosign
RUN chmod +x /usr/local/bin/gocdnext-cosign

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-cosign"]
