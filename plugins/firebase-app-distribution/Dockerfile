# gocdnext/firebase-app-distribution — distribute an Android build (.apk/.aab)
# to testers via Firebase App Distribution, using the Firebase CLI authenticated
# with a service account. Pairs with the `android` plugin.
#
# Contract (PLUGIN_* env) — see entrypoint.sh. The service-account JSON (from
# secrets:) is written to a 0600 tempfile and exposed via
# GOOGLE_APPLICATION_CREDENTIALS; it never reaches argv or the logs.
FROM node:20-bookworm-slim

# Pinned Firebase CLI (Renovate bumps patch/minor; major pinned).
RUN npm install -g --no-fund --no-audit firebase-tools@13.29.1 \
    && npm cache clean --force

COPY entrypoint.sh /usr/local/bin/gocdnext-firebase-app-distribution
RUN chmod +x /usr/local/bin/gocdnext-firebase-app-distribution

WORKDIR /workspace
ENTRYPOINT ["/usr/local/bin/gocdnext-firebase-app-distribution"]
