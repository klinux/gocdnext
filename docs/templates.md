# Pipeline templates

Complete `.gocdnext/*.yaml` files for common stacks. Copy, rename, tweak.
Every template demonstrates the idioms we recommend: cache blocks keyed by
branch, `test_reports:` wired to the junit output the language plugin already
emits, `secrets:` piped through `${{ NAME }}` so plaintext never touches the
log, and an `approval:` gate before anything that promotes to production.

Pick the one closest to your stack and start from there.

---

## Next.js web app

Three stages: **build**, **test** (unit + Playwright e2e sharded 2-way), and
**deploy** gated by an approval. Node tool caches land in the workspace via
the `gocdnext/node` + `gocdnext/playwright` plugins — same cache key covers
both. Artifacts ship the Playwright HTML report so a failed e2e always has
a trace to click into.

```yaml
# .gocdnext/web.yaml — Next.js app (pnpm) deployed to S3 + CloudFront.
name: web

materials:
  - git:
      url: https://github.com/org/my-web
      branch: main
      on: [push, pull_request]
      auto_register_webhook: true

stages: [build, test, deploy]

variables:
  NODE_VERSION: "20"

jobs:
  install:
    stage: build
    cache:
      - key: pnpm-store-${CI_COMMIT_BRANCH}
        paths:
          - web/.pnpm-store
          - web/node_modules
    uses: gocdnext/node@v1
    with:
      working-dir: web
      command: install --frozen-lockfile

  build:
    stage: build
    needs: [install]
    cache:
      - key: pnpm-store-${CI_COMMIT_BRANCH}
        paths: [web/.pnpm-store, web/node_modules]
    uses: gocdnext/node@v1
    with:
      working-dir: web
      command: run build
    artifacts:
      paths: [web/.next, web/out]

  typecheck:
    stage: test
    needs: [install]
    cache:
      - key: pnpm-store-${CI_COMMIT_BRANCH}
        paths: [web/.pnpm-store, web/node_modules]
    uses: gocdnext/node@v1
    with:
      working-dir: web
      command: exec tsc --noEmit

  e2e:
    stage: test
    needs: [build]
    parallel:
      matrix:
        - SHARD: ["1", "2"]
    cache:
      - key: pnpm-store-${CI_COMMIT_BRANCH}
        paths: [web/.pnpm-store, web/node_modules]
    uses: gocdnext/playwright@v1
    with:
      working-dir: web
      project: chromium
      shard: "${SHARD}/2"
      install-deps: "false"
    test_reports:
      - web/test-results/junit.xml
    artifacts:
      optional:
        - web/playwright-report
        - web/test-results

  approve-prod:
    stage: deploy
    approval:
      approvers: [alice, bob]
      description: "Deploy main to prod?"

  deploy-prod:
    stage: deploy
    needs: [approve-prod]
    needs_artifacts:
      - from_job: build
        paths: [web/out]
    secrets:
      - AWS_ACCESS_KEY_ID
      - AWS_SECRET_ACCESS_KEY
    uses: gocdnext/s3@v1
    with:
      action: upload
      bucket: mycorp-web-prod
      key: releases/${CI_COMMIT_SHORT_SHA}/
      file: web/out
      region: us-east-1

notifications:
  - on: failure
    uses: gocdnext/slack@v1
    with:
      webhook: ${{ SLACK_WEBHOOK }}
      channel: "#eng"
    secrets: [SLACK_WEBHOOK]
  - on: success
    uses: gocdnext/slack@v1
    with:
      webhook: ${{ SLACK_WEBHOOK }}
      channel: "#releases"
      message: "web #${CI_RUN_COUNTER} shipped ${CI_COMMIT_SHORT_SHA}"
    secrets: [SLACK_WEBHOOK]
```

---

## Go microservice

Container-first service: build a static binary, run tests with race detector,
publish a multi-arch image via `buildx`, deploy via `kubectl` with an approval
gate on prod. Module + build caches ride along for free — the `gocdnext/go`
plugin already redirects `$GOMODCACHE` + `$GOCACHE` into the workspace.

```yaml
# .gocdnext/service.yaml — Go microservice → GHCR → k8s.
name: service

materials:
  - git:
      url: https://github.com/org/my-service
      branch: main
      on: [push, pull_request]
      auto_register_webhook: true

stages: [test, build, deploy]

jobs:
  test:
    stage: test
    cache:
      - key: go-${CI_COMMIT_BRANCH}
        paths: [.go-mod, .go-cache]
    uses: gocdnext/go@v1
    with:
      command: test -race -cover -coverprofile=cover.out ./...
    test_reports:
      - junit.xml
    artifacts:
      optional: [cover.out]

  coverage:
    stage: test
    needs: [test]
    needs_artifacts:
      - from_job: test
        paths: [cover.out]
    secrets: [CODECOV_TOKEN]
    uses: gocdnext/codecov@v1
    with:
      file: cover.out
      flags: unit
      token: ${{ CODECOV_TOKEN }}

  vet:
    stage: test
    cache:
      - key: go-${CI_COMMIT_BRANCH}
        paths: [.go-mod, .go-cache]
    uses: gocdnext/go@v1
    with:
      command: vet ./...

  image:
    stage: build
    needs: [test, vet]
    docker: true
    secrets:
      - GHCR_USERNAME
      - GHCR_TOKEN
    uses: gocdnext/buildx@v1
    with:
      image: ghcr.io/org/my-service
      tags: "${CI_COMMIT_SHORT_SHA}, latest"
      platforms: linux/amd64,linux/arm64
      username: ${{ GHCR_USERNAME }}
      password: ${{ GHCR_TOKEN }}

  scan:
    stage: build
    needs: [image]
    uses: gocdnext/trivy@v1
    with:
      image: ghcr.io/org/my-service:${CI_COMMIT_SHORT_SHA}
      severity: HIGH,CRITICAL
      exit-code: "1"

  approve-prod:
    stage: deploy
    approval:
      approvers: [alice, bob]
      description: "Promote ${CI_COMMIT_SHORT_SHA} to prod?"

  deploy-prod:
    stage: deploy
    needs: [approve-prod]
    secrets: [KUBECONFIG_PROD]
    uses: gocdnext/kubectl@v1
    with:
      kubeconfig: ${{ KUBECONFIG_PROD }}
      command: |
        set image deployment/my-service \
          my-service=ghcr.io/org/my-service:${CI_COMMIT_SHORT_SHA} \
          -n prod
      namespace: prod

notifications:
  - on: failure
    uses: gocdnext/slack@v1
    with:
      webhook: ${{ SLACK_WEBHOOK }}
      channel: "#oncall"
    secrets: [SLACK_WEBHOOK]
```

---

## Python library

Test + build + publish to PyPI, with Codecov coverage and a release flow
fired by pushing a tag. `gocdnext/python` auto-detects the manager (poetry
here) and redirects the tool cache into the workspace. `gocdnext/release-notes`
generates the changelog from the commit history since the last tag.

```yaml
# .gocdnext/lib.yaml — Python library → PyPI.
name: lib

materials:
  - git:
      url: https://github.com/org/my-lib
      branch: main
      on: [push, pull_request]
      auto_register_webhook: true

stages: [test, build, publish]

jobs:
  test:
    stage: test
    parallel:
      matrix:
        - PYVER: ["3.11", "3.12"]
    cache:
      - key: py-${CI_COMMIT_BRANCH}-${PYVER}
        paths: [.cache]
    uses: gocdnext/python@v1
    with:
      command: pytest -q --junitxml=junit.xml --cov --cov-report=xml
    test_reports:
      - junit.xml

  coverage:
    stage: test
    needs: [test]
    secrets: [CODECOV_TOKEN]
    uses: gocdnext/codecov@v1
    with:
      file: coverage.xml
      flags: unit
      token: ${{ CODECOV_TOKEN }}

  build:
    stage: build
    needs: [test]
    cache:
      - key: py-${CI_COMMIT_BRANCH}-3.12
        paths: [.cache]
    uses: gocdnext/python@v1
    with:
      command: poetry build
    artifacts:
      paths: [dist/]

  changelog:
    stage: build
    needs: [test]
    uses: gocdnext/release-notes@v1
    with:
      format: conventional
      output: CHANGELOG.md
      heading: "## ${CI_COMMIT_SHORT_SHA}"
    artifacts:
      optional: [CHANGELOG.md]

  approve-release:
    stage: publish
    approval:
      approvers: [alice]
      description: "Cut a PyPI release from ${CI_COMMIT_SHORT_SHA}?"

  pypi:
    stage: publish
    needs: [approve-release, build]
    needs_artifacts:
      - from_job: build
        paths: [dist/]
    secrets: [PYPI_TOKEN]
    uses: gocdnext/python@v1
    with:
      command: |
        poetry publish --username __token__ --password ${PYPI_TOKEN} --no-interaction

  tag:
    stage: publish
    needs: [pypi]
    secrets: [RELEASE_TOKEN]
    uses: gocdnext/tag@v1
    with:
      name: v${RELEASE_VERSION}
      message: "Release v${RELEASE_VERSION}"
      username: x-access-token
      token: ${{ RELEASE_TOKEN }}

notifications:
  - on: success
    uses: gocdnext/email@v1
    with:
      host: smtp.sendgrid.net
      port: "587"
      username: ${{ SMTP_USER }}
      password: ${{ SMTP_PASSWORD }}
      from: "Releases <ci@mycorp.com>"
      to: "releases@mycorp.com"
      subject: "my-lib v${RELEASE_VERSION} shipped"
      body: |
        Commit: ${CI_COMMIT_SHA}
        Changelog: attached in the run artifacts
        Run: ${CI_RUN_URL}
    secrets: [SMTP_USER, SMTP_PASSWORD]
  - on: failure
    uses: gocdnext/slack@v1
    with:
      webhook: ${{ SLACK_WEBHOOK }}
      channel: "#eng"
    secrets: [SLACK_WEBHOOK]
```

---

## Where to go next

- [Pipeline spec](/docs/pipeline-spec) — full reference for every field.
- [Plugins catalogue](/plugins) — 35 plugins ready to drop into a `uses:`.
- [Architecture](/docs/architecture) — the big picture of runs, agents, and
  the scheduler behind the YAML.
