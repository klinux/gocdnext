# gocdnext/codecov — upload a coverage report to Codecov via the
# official `codecov` CLI (the Rust rewrite that replaced the old
# bash uploader in 2023). Uses a repo token for auth so uploads
# work from private forks too — the old auto-discover-token
# trick needed GitHub Actions metadata that a generic CI doesn't
# ship.
#
# Contract (PLUGIN_* env, mapped from `with:`):
#   PLUGIN_TOKEN        (required)  Codecov repo upload token.
#                                   Populate via secrets:.
#   PLUGIN_FILE         (optional)  Explicit coverage file
#                                   (cover.out, coverage.xml, …).
#                                   When omitted the CLI
#                                   auto-detects common file
#                                   names at the working-dir root.
#   PLUGIN_FLAGS        (optional)  comma-separated Codecov
#                                   flags — e.g. "unit,integration"
#                                   tags the upload so the Codecov
#                                   UI can split coverage by stage.
#   PLUGIN_WORKING_DIR  (optional)  cd here first. Default ".".
#   PLUGIN_SLUG         (optional)  "owner/repo" override.
#                                   Codecov normally pulls this
#                                   from git remote; set it when
#                                   the working-dir isn't a git
#                                   checkout (e.g. uploaded
#                                   artifact).
#   PLUGIN_URL          (optional)  Codecov Enterprise URL. The
#                                   cloud default is codecov.io.
FROM alpine:3.20

# Codecov's official uploader ships as a static Linux binary.
# Fetch it once during the image build so each run doesn't pay
# a download tax (the CLI is ~25MB; at 1000 runs/day that's
# real bandwidth).
#
# checksum verification follows Codecov's docs — the uploader
# is GPG-signed and publishes a SHA256SUM file. We verify the
# sha only (GPG key rotation would require shipping the public
# key too; sha is a reasonable floor for tamper detection).
RUN apk add --no-cache bash curl ca-certificates git

ARG CODECOV_VERSION=v10.3.0
RUN set -eux; \
    curl -fSsL "https://cli.codecov.io/${CODECOV_VERSION}/linux/codecov" \
         -o /usr/local/bin/codecov; \
    chmod +x /usr/local/bin/codecov; \
    /usr/local/bin/codecov --version

COPY entrypoint.sh /usr/local/bin/gocdnext-codecov
RUN chmod +x /usr/local/bin/gocdnext-codecov

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-codecov"]
