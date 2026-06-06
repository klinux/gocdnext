---
title: Security scanning (trivy + gitleaks)
description: Catch secrets in the repo and CVEs in container images before they hit production.
---

Two scans every project should have: **gitleaks** for secrets
checked into the repo by accident, **trivy** for CVEs in the
images you ship. Both are cheap to run, both fail loud when
something's wrong, both are plugins.

## The pipeline

```yaml title=".gocdnext/security.yaml"
name: security

when:
  event: [push, pull_request]

stages: [scan]

jobs:
  gitleaks:
    stage: scan
    uses: ghcr.io/klinux/gocdnext-plugin-gitleaks@v1
    with:
      # Default ruleset catches the common patterns (AWS keys, GCP
      # service-account JSON, GitHub PATs, Slack webhooks, …).
      # Add custom rules via .gitleaks.toml + the `config:` input.
      scan-mode: dir              # working tree only (fast); use "git" for full history
      path: .
      exit-code: "1"              # fail on any finding
      verbose: "true"
      redact: "75"                # mask 75% of each finding in the log

  trivy-fs:
    stage: scan
    uses: ghcr.io/klinux/gocdnext-plugin-trivy@v1
    with:
      scan-type: fs
      target: .
      severity: HIGH,CRITICAL
      exit-code: "1"
      ignore-unfixed: "true"

  trivy-config:
    stage: scan
    uses: ghcr.io/klinux/gocdnext-plugin-trivy@v1
    with:
      scan-type: config
      target: ./k8s
      severity: HIGH,CRITICAL
      exit-code: "1"
```

What's worth highlighting:

### `scan-mode: dir` for current state only

The default `scan-mode: dir` scans the working tree — what's
actually about to ship. `scan-mode: git` walks the full commit
history and is useful for one-shot historical audits, but
expensive on every push.

For a historical sweep: run `gitleaks detect --source .` locally
on the repo + commit the cleanup. The CI job catches new leaks;
the local sweep catches the old ones.

### Three trivy scan types

- `scan-type: fs` — scans `target:` (a directory) for vulnerable
  dependencies in lockfiles (package-lock.json, go.sum,
  Gemfile.lock, etc.). What you run on every push.
- `scan-type: image` — scans a built container. Used in the
  [Docker build recipe](/gocdnext/docs/pipelines/recipes/docker-build/).
  Needs `docker: true`.
- `scan-type: config` — scans Kubernetes manifests, Dockerfiles,
  Terraform for misconfigurations (privileged: true, root user,
  hardcoded secrets, etc.). What you run on infra repos.

Trivy doesn't accept multiple `target:` paths in one job — pass a
single directory and trivy walks it. For separate sweeps over
multiple trees (k8s + terraform + Dockerfiles), declare one job
per target.

### `severity: HIGH,CRITICAL` is the right default

Lowering to `MEDIUM` or `LOW` adds noise. Most non-critical
findings are false-positive-prone (CVE-in-a-test-dep, etc.).
Start strict; lower the bar only when the team is ready to
triage the volume.

### `exit-code: "1"` blocks the run

Without it, trivy reports findings to the log but the job
returns 0 (success). `"1"` (the default) fails the run on any
matching severity, which is what you want — security findings
should block merge.

For audit-only runs (no blocking, just visibility), set
`exit-code: "0"` and surface the report as an artefact:

```yaml
trivy-fs:
  stage: scan
  uses: ghcr.io/klinux/gocdnext-plugin-trivy@v1
  with:
    scan-type: fs
    target: .
    severity: HIGH,CRITICAL,MEDIUM,LOW
    exit-code: "0"
    output: trivy-report.json
    format: json
  artifacts:
    paths: [trivy-report.json]
```

## Variations

### Allowlist docs + fixtures (gitleaks)

Known-safe paths that legitimately ship example tokens get
allowlisted at the plugin level rather than committing a
`.gitleaks.toml`. Composes with `config:` if you have project-
specific rules.

```yaml
gitleaks:
  stage: scan
  uses: ghcr.io/klinux/gocdnext-plugin-gitleaks@v1
  with:
    scan-mode: dir
    allowlist-paths: "docs/, test/fixtures/, examples/"
    verbose: "false"             # keep the build log quiet on clean repos
```

### SBOM generation (CycloneDX)

```yaml
sbom:
  stage: scan
  uses: ghcr.io/klinux/gocdnext-plugin-trivy@v1
  with:
    scan-type: fs
    target: .
    output: sbom.cdx.json
    format: cyclonedx
    exit-code: "0"
  artifacts:
    paths: [sbom.cdx.json]
```

The CycloneDX SBOM lists every package with its version + license
+ CPE identifier. Compliance tooling consumes it directly.

### Persist trivy DB cache across runs

trivy downloads its vulnerability DB at start. Cache it to avoid
re-downloading every run; pair with `skip-db-update: "true"` on
runs that should be fully offline (air-gapped agents).

```yaml
trivy-fs:
  stage: scan
  uses: ghcr.io/klinux/gocdnext-plugin-trivy@v1
  cache:
    - key: trivy-db
      paths: [.cache/trivy]
  with:
    scan-type: fs
    target: .
    skip-db-update: "true"        # rely on the cached DB
```

### Notify on findings (Slack)

Pair trivy with the [notifications recipe](/gocdnext/docs/pipelines/recipes/notifications/)
to ping a Slack channel when a scan job fails. Note the
`secrets:` declaration and `${{ NAME }}` identifier-only ref —
dotted `${{ secrets.X }}` is rejected.

```yaml
notifications:
  - on: failure
    uses: ghcr.io/klinux/gocdnext-plugin-slack@v1
    with:
      webhook: ${{ SECURITY_SLACK_WEBHOOK }}
      channel: "#security-alerts"
      template: ":rotating_light: Security scan failed on ${CI_PIPELINE} #${CI_RUN_COUNTER}"
    secrets: [SECURITY_SLACK_WEBHOOK]
```

## Common pitfalls

- **gitleaks false positives**: add to `.gitleaksignore` (commit
  hash + rule ID). The plugin respects the standard config.
  Don't disable rules globally — pin per-finding. Allowlist
  whole paths via the plugin's `allowlist-paths:` input when
  the directory legitimately ships example creds.
- **trivy DB updates**: the plugin downloads the vulnerability DB
  on each run. First-time runs take ~30s; subsequent ones hit
  the cache. Persist `.cache/trivy` across runs (see variation
  above) on networks where the download cost matters.
- **`scan-type: config` on Helm output**: trivy can't read raw
  Helm templates; they need to be `helm template`'d first. Add
  a render step that emits to `dist/manifests/` and point trivy
  at that.
- **CVE allowlists**: when a CVE is unfixable in your context,
  add it to `.trivyignore` with an explanation comment. PRs
  that touch this file should be reviewed by security — not just
  any maintainer.
