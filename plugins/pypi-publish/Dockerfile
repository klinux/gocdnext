# gocdnext/pypi-publish — upload built distributions (dist/*) to
# PyPI or a compatible index via twine.
#
# Contract:
#   PYPI_TOKEN          (required)  API token via the job's
#                                   `secrets:` (twine env auth —
#                                   never argv).
#   PLUGIN_DIST_DIR     (optional)  default dist/.
#   PLUGIN_REPOSITORY_URL (optional) default PyPI upload endpoint.
#   PLUGIN_SKIP_EXISTING (optional) twine --skip-existing
#                                   (idempotent retries).
#   PLUGIN_CHECK_ONLY   (optional)  twine check only (PR preflight,
#                                   no token needed).
#
# Building the distribution is the BUILD job's responsibility
# (python -m build) — this plugin uploads what `dist/` already
# holds, shipped over via artifacts. Note on trusted publishing:
# PyPI's OIDC exchange only accepts an allowlist of issuers
# (GitHub, GitLab, Google, ActiveState today) — gocdnext id_tokens
# can't mint PyPI tokens until PyPI accepts custom issuers, so v1
# is token-based.
FROM python:3.12-alpine

RUN pip install --no-cache-dir twine==6.1.0

COPY entrypoint.sh /usr/local/bin/gocdnext-pypi-publish
RUN chmod +x /usr/local/bin/gocdnext-pypi-publish

WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/gocdnext-pypi-publish"]
