# gocdnext/gravitee — configure a Gravitee.io API definition through the
# graviteeio-cli (`gio`). CLI-only; no docker daemon needed.
#
# Contract (PLUGIN_* env, mapped from `with:`; secrets referenced via ${{ }}):
#   PLUGIN_API_NAME   (required)  API name in Gravitee (id lookup + render).
#   PLUGIN_URL        (required)  Management API base URL  -> GIO_APIM_URL.
#   PLUGIN_TOKEN      (required)  bearer token -> GIO_APIM_TOKEN (env, never argv).
#   PLUGIN_PATH       (opt, ".")  def-path: the API config folder.
#   PLUGIN_VALUES_FILE(opt,api.yml) per-API values, overlaid on defaults.
#   PLUGIN_DEFAULTS   (opt)       base values file, path OR URL.
#   PLUGIN_TEMPLATE   (opt)       api_config.yml.j2 template, path OR URL.
#   PLUGIN_MODE       (opt,merge) merge (deep, arrays concat) | overwrite.
#   PLUGIN_DEPLOY     (opt,true)  deploy after apply (--with-deploy/--with-start).
#   PLUGIN_LINT       (opt,true)  `gio … definition lint` before writing.
#   PLUGIN_MANAGE_PLANS_ON_UPDATE (opt,false) DANGER: true lets the import
#                                 reconcile plans on update (can break
#                                 active subscriptions). Default false =
#                                 strip plans from the payload (safe).
#   PLUGIN_METHOD_POLICY (opt,warn) off|warn|block — gate paths whose rules
#                                 don't declare explicit methods (open under
#                                 the keyless plan).
#   PLUGIN_AUTH_POLICY   (opt,off)  off|warn|block — gate methods with no
#                                 auth policy (reachable unauthenticated).
#   PLUGIN_AUTH_POLICIES (opt)     csv of policy names that count as auth
#                                 (default oauth2,jwt,api-key).
#   PLUGIN_ORG/PLUGIN_ENV (opt)   GIO_APIM_ORG / GIO_APIM_ENV.
#   PLUGIN_SSL_VERIFY (opt,true)  GIO_SSL_VERIFY (true/false/path).
#   PLUGIN_CA_BUNDLE  (opt)       PEM/base64 CA -> pinned TLS verify.
#   PLUGIN_CONFIG_TOKEN (opt)     bearer for fetching defaults/template URLs.
#
# Debian slim (not alpine) because the entrypoint is bash and gio targets
# CPython 3.7–3.9 — so Python stays on 3.9 (bumping it risks the gio fork).
# Base pinned by digest; `apt-get upgrade` below applies Debian security
# backports at build time (the published :v1 had carried stale OpenSSL/GnuTLS
# CRITICALs because its apt layer was cache-frozen). The plugin opts into
# `no_cache` in plugin.yaml so the publish build can't re-import that stale layer.
#
# NOTE: `apt-get upgrade` pulls Debian's MUTABLE security repos, so the OS set is
# patched at build time, not byte-identical build-to-build; the FROM digest pins
# only the base layer, and the weekly Trivy gate catches drift.
FROM python:3.9-slim-bookworm@sha256:a02e9c5406c416c504d6c9a1a306ff4080c3173f1008d192f953bd20382a2d5c

# graviteeio-cli: the klinux-maintained fork (graviteeio-cli 0.4 + fixes) — it
# accepts a Resource `configuration` OBJECT in the gio_apim definition schema
# (upstream 0.4 requires a string and rejects the object form real Gravitee
# definitions use, e.g. an oauth2 resource), plus a pathlib fix and an optional
# TLS-verify skip. Source + attribution: https://github.com/klinux/gravitee-cli
# (Apache-2.0).
#
# The WHOLE Python tree is pinned by a hash-locked requirements.txt (the fork
# wheel's URL + every transitive dep, each with sha256) installed via
# `pip --require-hashes`, so `no_cache` rebuilds resolve the exact same tree and
# can't silently pull a changed transitive dep. Regenerate after bumping gio:
#   pip-compile --generate-hashes --allow-unsafe -o requirements.txt requirements.in
# mikefarah yq v4 for the values merge (deep merge + array concat),
# pinned + checksum-verified so the binary can't change under us. Bumped to
# v4.54.2 (built with Go 1.27.3) to clear the Go-stdlib CVE-2025-68121 that
# v4.44.3 carried (it was built with Go 1.22).
ARG YQ_VERSION="v4.54.2"
ARG YQ_SHA256="44c7759059b4910827a0059a2757b22001e59360cdecdb2ef8dedc4b6c7e9f32"

COPY requirements.txt /tmp/requirements.txt
RUN set -eux; \
    apt-get update; \
    apt-get upgrade -y; \
    apt-get install -y --no-install-recommends \
        ca-certificates curl gettext-base jq git build-essential; \
    pip install --no-cache-dir --require-hashes -r /tmp/requirements.txt; \
    rm -f /tmp/requirements.txt; \
    curl -fsSL -o /usr/local/bin/yq \
        "https://github.com/mikefarah/yq/releases/download/${YQ_VERSION}/yq_linux_amd64"; \
    echo "${YQ_SHA256}  /usr/local/bin/yq" | sha256sum -c -; \
    chmod +x /usr/local/bin/yq; \
    apt-get purge -y --auto-remove build-essential git; \
    rm -rf /var/lib/apt/lists/*; \
    gio --version

COPY entrypoint.sh /usr/local/bin/gocdnext-gravitee
COPY gravitee_validate.py /usr/local/bin/gravitee-validate
RUN chmod +x /usr/local/bin/gocdnext-gravitee /usr/local/bin/gravitee-validate

WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/gocdnext-gravitee"]
