# gocdnext/vercel — deploy to Vercel.
#
# Contract:
#   VERCEL_TOKEN       (required)  via `secrets:`.
#   VERCEL_ORG_ID / VERCEL_PROJECT_ID (required via `secrets:` or
#                                  variables — the CLI's headless
#                                  project linkage).
#   PLUGIN_PROD        (optional)  production vs preview.
#   PLUGIN_PREBUILT    (optional)  deploy .vercel/output produced
#                                  by `vercel build` in a prior job.
FROM node:22-alpine

RUN npm install -g vercel@37 && npm cache clean --force

COPY entrypoint.sh /usr/local/bin/gocdnext-vercel
RUN chmod +x /usr/local/bin/gocdnext-vercel

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-vercel"]
