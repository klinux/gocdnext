# gocdnext/netlify — deploy a built directory to Netlify.
#
# Contract:
#   NETLIFY_AUTH_TOKEN (required)  via `secrets:` — the CLI reads
#                                  it natively from env.
#   PLUGIN_DIR         (required)  the built site directory.
#   PLUGIN_SITE_ID     (required)  Netlify site (or set
#                                  NETLIFY_SITE_ID via secrets).
#   PLUGIN_PROD        (optional)  production deploy vs preview.
#   PLUGIN_ALIAS       (optional)  preview alias (pr-N etc).
FROM node:22-alpine

RUN npm install -g netlify-cli@17 && npm cache clean --force

COPY entrypoint.sh /usr/local/bin/gocdnext-netlify
RUN chmod +x /usr/local/bin/gocdnext-netlify

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-netlify"]
