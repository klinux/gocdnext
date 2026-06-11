# gocdnext/cloudflare-pages — deploy a built directory to
# Cloudflare Pages via wrangler.
#
# Contract:
#   CLOUDFLARE_API_TOKEN  (required)  via `secrets:` — wrangler
#                                     reads it natively from env.
#   CLOUDFLARE_ACCOUNT_ID (required)  via `secrets:` or variables.
#   PLUGIN_DIR            (required)  the built site directory.
#   PLUGIN_PROJECT_NAME   (required)  Pages project.
#   PLUGIN_BRANCH         (optional)  deployment branch (Pages maps
#                                     non-production branches to
#                                     preview URLs).
FROM node:22-alpine

RUN npm install -g wrangler@3 && npm cache clean --force

COPY entrypoint.sh /usr/local/bin/gocdnext-cloudflare-pages
RUN chmod +x /usr/local/bin/gocdnext-cloudflare-pages

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-cloudflare-pages"]
