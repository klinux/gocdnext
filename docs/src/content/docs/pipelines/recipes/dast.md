---
title: DAST (deploy-then-scan with Nuclei)
description: Boot a running app and scan it dynamically — baseline + API-spec-driven — with findings flowing into the Security dashboard.
---

The other scanners read your **code, deps, images and manifests**. **DAST**
(Dynamic Application Security Testing) scans a **running** app: it sends real
requests and looks at real responses. gocdnext makes the usually-hard part —
*having a running target in CI* — easy with [`services:`](/gocdnext/docs/concepts/services/),
and the [nuclei plugin](/gocdnext/docs/reference/plugins/#nuclei) emits SARIF so
findings land in the per-project [Security dashboard](/gocdnext/docs/concepts/security/).

Scope, set honestly: **v1 is an unauthenticated baseline + API-spec-driven
scan**. That's genuinely useful out of the box. Authenticated/deep scans are
config-heavy in *every* DAST tool — see [Authenticated scans](#authenticated-scans-advanced).

## 1. Ephemeral target via `services:` (the easy win)

Boot the app you just built as a service and point nuclei at it. The plugin
**preflights** the target (waits for it to come up) before scanning, so a
still-booting app fails loud instead of recording a false "clean".

```yaml title=".gocdnext/dast.yaml"
name: dast

when:
  event: [pull_request]

services:
  app:
    image: ghcr.io/acme-org/app:${CI_COMMIT_SHORT_SHA}
    ports: [8080]

stages: [scan]

jobs:
  nuclei:
    stage: scan
    uses: ghcr.io/klinux/gocdnext-plugin-nuclei@v1
    with:
      target: http://app:8080      # the service is reachable by name on the run network
      health-path: /healthz        # readiness gate (requires 2xx/3xx)
      severity: critical,high,medium
      fail-on: critical,high       # report medium, block on high/critical
    artifacts:
      - nuclei.sarif               # ingested into the Security tab
```

## 2. API-spec-driven (deeper than baseline)

Point nuclei at your OpenAPI/Swagger spec for endpoint-aware coverage. The
spec's base URL (`servers:` / `host`) is **rewritten to `target`** before
scanning — so a spec that ships `servers: https://api.prod...` can never pull the
scan onto production.

```yaml
jobs:
  nuclei-api:
    stage: scan
    uses: ghcr.io/klinux/gocdnext-plugin-nuclei@v1
    with:
      target: http://app:8080
      spec: openapi.yaml           # OpenAPI 3 or Swagger 2, YAML or JSON
    artifacts:
      - nuclei.sarif
```

## 3. Ephemeral env via Kubernetes

When `services:` isn't enough — you need a real deploy (ingress, DB migrations,
sidecars) — deploy to a throwaway namespace, scan its URL, then tear it down.
The "decent coverage" of DAST comes from this surrounding recipe, not just the
wrapper.

```yaml
stages: [deploy, scan, teardown]

jobs:
  deploy-ephemeral:
    stage: deploy
    uses: ghcr.io/klinux/gocdnext-plugin-helm@v1
    with:
      args: upgrade --install pr-${CI_RUN_COUNTER} ./chart
        --namespace pr-${CI_RUN_COUNTER} --create-namespace
        --set image.tag=${CI_COMMIT_SHORT_SHA} --wait

  nuclei:
    stage: scan
    uses: ghcr.io/klinux/gocdnext-plugin-nuclei@v1
    with:
      # Prefer an in-cluster / http URL — see the TLS note below.
      target: http://app.pr-${CI_RUN_COUNTER}.svc.cluster.local
      ready-timeout: "120"
    artifacts:
      - nuclei.sarif

  teardown:
    stage: teardown
    when: always               # always clean up the namespace
    uses: ghcr.io/klinux/gocdnext-plugin-kubectl@v1
    with:
      args: delete namespace pr-${CI_RUN_COUNTER} --ignore-not-found
```

:::note[TLS / internal CA]
Prefer `http://` or in-cluster URLs (or a publicly-trusted cert) for ephemeral
scans. An `https://` target with an **internal/self-signed CA** fails preflight
loud (`curl` → `000`). TLS-skip is intentionally not a v1 default — it belongs in
an advanced recipe, not a casual input.
:::

## 4. Enforce via compliance (`_compliance_dast`)

Admins can make DAST **mandatory** with a [compliance policy](/gocdnext/docs/concepts/compliance/).
The reserved `_compliance_` prefix means a repo can't shadow or skip it:

```yaml
# in a compliance policy
stages: [_compliance_scan]
jobs:
  _compliance_dast:
    stage: _compliance_scan
    uses: ghcr.io/klinux/gocdnext-plugin-nuclei@v1
    with:
      target: http://app:8080
      fail-on: critical,high
    artifacts:
      - nuclei.sarif
```

## Authenticated scans (advanced)

Authenticated DAST is genuinely harder — session handling, CSRF tokens, login
flows — and config-heavy in every tool (GitLab DAST included). The nuclei plugin
v1 deliberately carries **no auth/header input** (a token on a scanner's argv is
a leak waiting to happen). For authenticated coverage, drive the
[OWASP ZAP automation framework](https://www.zaproxy.org/docs/automate/automation-framework/)
with an auth context as a dedicated job, or scope nuclei to a pre-authenticated
reverse proxy you stand up in the recipe. Treat it as an advanced, per-app
exercise — not a single magic input.

## Common pitfalls

- **Target not up** → the scan fails (exit 2), by design — it won't record a
  false clean. Tune `ready-timeout` / `health-path` for slow-booting apps.
- **`fail-on` never fires** → it must be a subset of `severity` (the plugin
  errors loudly if not). Report wide, block narrow.
- **Too aggressive for a tiny app** → lower `rate-limit` (default 50) further.
- **OAST** (out-of-band) is off by default; enable with `interactsh: true` only
  when your runner has the egress for it.
