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
# CPython 3.7–3.9.
FROM python:3.9-slim-bookworm

# Which graviteeio-cli to install. Pinned to the validated upstream
# version for a reproducible build; override at build time if you
# maintain a fork (e.g. for a TLS-verify or packaging fix), pinned to a
# tag/commit — the override keeps any private fork URL out of this
# public manifest:
#   docker build \
#     --build-arg GRAVITEE_CLI_SPEC="git+https://github.com/<your-org>/gravitee-cli@<tag-or-sha>"
ARG GRAVITEE_CLI_SPEC="graviteeio-cli==0.4"
# mikefarah yq v4 for the values merge (deep merge + array concat),
# pinned + checksum-verified so the binary can't change under us.
ARG YQ_VERSION="v4.44.3"
ARG YQ_SHA256="a2c097180dd884a8d50c956ee16a9cec070f30a7947cf4ebf87d5f36213e9ed7"

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ca-certificates curl gettext-base jq git build-essential; \
    pip install --no-cache-dir "${GRAVITEE_CLI_SPEC}"; \
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
