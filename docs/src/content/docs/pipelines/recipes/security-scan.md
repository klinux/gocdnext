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
    uses: gocdnext/gitleaks@v1
    with:
      # Default config covers the common patterns (AWS keys, GCP
      # service-account JSON, GitHub PATs, Slack webhooks, …).
      # Add custom rules in .gitleaks.toml at the repo root.
      args: detect --source . --redact --no-git --verbose

  trivy-fs:
    stage: scan
    uses: gocdnext/trivy@v1
    with:
      scan_type: fs
      target: .
      severity: HIGH,CRITICAL
      exit_code: 1
      ignore_unfixed: true
      # Skip vendored deps that ship their own CVE db (rare). Most
      # repos don't need this.
      skip_dirs: vendor,node_modules

  trivy-config:
    stage: scan
    uses: gocdnext/trivy@v1
    with:
      scan_type: config
      target: ./k8s ./terraform ./Dockerfile
      severity: HIGH,CRITICAL
      exit_code: 1
```

What's worth highlighting:

### `gitleaks --no-git` for current state only

Without `--no-git`, gitleaks walks the entire git history. That's
ideal when you suspect an old leak; for CI it's overkill — the
current commit is what's about to ship. `--no-git` scans the
working tree only, which is what `event: [push, pull_request]`
should care about.

For a one-shot historical audit: run `gitleaks detect --source .`
locally + commit the cleanup. The CI job catches new leaks; the
local sweep catches the old ones.

### Three trivy scan types

- `scan_type: fs` — scans `target:` (a directory) for vulnerable
  dependencies in lockfiles (package-lock.json, go.sum, Gemfile.lock,
  etc.). What you run on every push.
- `scan_type: image` — scans a built container. Used in the
  [Docker build recipe](/gocdnext/docs/pipelines/recipes/docker-build/).
  Needs `docker: true`.
- `scan_type: config` — scans Kubernetes manifests, Dockerfiles,
  Terraform for misconfigurations (privileged: true, root user,
  hardcoded secrets, etc.). What you run on infra repos.

### `severity: HIGH,CRITICAL` is the right default

Lowering to `MEDIUM` or `LOW` adds noise. Most non-critical
findings are false-positive-prone (CVE-in-a-test-dep, etc.).
Start strict; lower the bar only when the team is ready to
triage the volume.

### `exit_code: 1` blocks the run

Without it, trivy reports findings to the log but the job
returns 0 (success). Default 1 fails the run on any matching
severity, which is what you want — security findings should
block merge.

For audit-only runs (no blocking, just visibility), set
`exit_code: 0` and surface the report as an artefact:

```yaml
trivy-fs:
  stage: scan
  uses: gocdnext/trivy@v1
  with:
    scan_type: fs
    target: .
    severity: HIGH,CRITICAL,MEDIUM,LOW
    exit_code: 0
    output: trivy-report.json
    format: json
  artifacts:
    paths: [trivy-report.json]
```

## Variations

### SBOM generation (CycloneDX)

```yaml
sbom:
  stage: scan
  uses: gocdnext/trivy@v1
  with:
    scan_type: fs
    target: .
    output: sbom.cdx.json
    format: cyclonedx
    exit_code: 0
  artifacts:
    paths: [sbom.cdx.json]
```

The CycloneDX SBOM lists every package with its version + license
+ CPE identifier. Compliance tooling consumes it directly.

### Block PRs but warn on push

Different `severity` thresholds per event:

```yaml
trivy-pr:
  stage: scan
  uses: gocdnext/trivy@v1
  when:
    event: [pull_request]
  with:
    scan_type: fs
    severity: HIGH,CRITICAL
    exit_code: 1               # block PRs

trivy-push:
  stage: scan
  uses: gocdnext/trivy@v1
  when:
    event: [push]
    branches: [main]
  with:
    scan_type: fs
    severity: HIGH,CRITICAL
    exit_code: 0               # main shouldn't ship without finding,
                               # but if it does we want visibility,
                               # not breakage
```

### Notify on findings (Slack)

Pair trivy with the [notifications recipe](/gocdnext/docs/pipelines/recipes/notifications/)
to ping a Slack channel when a scan job fails:

```yaml
notifications:
  - on: failure
    uses: gocdnext/slack@v1
    with:
      webhook: ${{ secrets.SECURITY_SLACK_WEBHOOK }}
      channel: "#security-alerts"
      title: "🚨 ${CI_PROJECT_SLUG} — security scan failed"
```

## Common pitfalls

- **gitleaks false positives**: add to `.gitleaksignore` (commit
  hash + rule ID). The plugin respects the standard config.
  Don't disable rules globally — pin per-finding.
- **trivy DB updates**: the plugin downloads the vulnerability DB
  on each run. First-time runs take ~30s; subsequent ones cache.
  Add a `cache:` block on `.trivy-cache` if your runs are
  frequent and the network is slow.
- **`scan_type: config` on Helm output**: trivy can't read raw
  Helm templates; they need to be `helm template`'d first. Add
  a render step that emits to `dist/manifests/` and point trivy
  at that.
- **CVE allowlists**: when a CVE is unfixable in your context,
  add it to `.trivyignore` with an explanation comment. PRs
  that touch this file should be reviewed by security — not just
  any maintainer.
