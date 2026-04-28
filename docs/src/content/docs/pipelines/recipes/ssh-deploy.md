---
title: Deploy to a VPS via SSH
description: Build artefact, ship over rsync, restart a systemd service — for the world the k8s primitive doesn't cover.
---

For VPS, bare-metal, edge boxes, or anything that doesn't run on
Kubernetes, the [`gocdnext/ssh`](/gocdnext/docs/reference/plugins/#ssh)
plugin covers the "rsync the binary, restart the service" pattern in
one job. This recipe builds a Go binary then deploys it.

## Prereqs (one-time per environment)

### Generate a deploy key

```bash
ssh-keygen -t ed25519 -f deploy_key -N "" -C "gocdnext deploy"
ssh-copy-id -i deploy_key.pub deploy@api.example.com
```

### Capture the host fingerprint

```bash
ssh-keyscan -t ed25519 -p 22 api.example.com > known_hosts
```

This protects against MITM. The plugin defaults to
`StrictHostKeyChecking=yes` and refuses to connect without the
known-hosts blob.

### Stash both as gocdnext secrets

In the dashboard: *Project → Secrets*

| Key | Value |
|---|---|
| `SSH_DEPLOY_KEY` | full content of `deploy_key` (the private key, not the .pub) |
| `SSH_KNOWN_HOSTS` | full content of `known_hosts` |

Both are AES-256-GCM-encrypted at rest and masked in run logs.

## The pipeline

```yaml title=".gocdnext/cd.yaml"
name: cd
when:
  event: [push]
  branches: [main]                          # only deploy from main

stages: [build, ship]

jobs:
  binary:
    stage: build
    uses: gocdnext/go@v1
    with:
      command: build -o dist/api-server ./cmd/api
    artifacts:
      paths: [dist/api-server]

  deploy:
    stage: ship
    uses: gocdnext/ssh@v1
    needs: [binary]
    needs_artifacts:
      - from_job: binary
        paths: [dist/api-server]
    with:
      host: api.example.com
      user: deploy
      key: ${{ secrets.SSH_DEPLOY_KEY }}
      known_hosts: ${{ secrets.SSH_KNOWN_HOSTS }}
      upload: dist/api-server
      target: /opt/api/
      script: |
        sudo systemctl restart api
        sudo systemctl status api --no-pager
```

What's happening:

1. `binary` job builds + uploads `dist/api-server` as an artefact.
2. `deploy` job pulls that artefact down via `needs_artifacts`, then
   the `gocdnext/ssh` plugin:
   - writes the key to `/tmp/.../id` with mode 600
   - validates the host key against `known_hosts`
   - rsyncs `dist/api-server` to `/opt/api/` on the remote (`--mkpath`
     creates the dir if missing)
   - opens an SSH session and runs the script under
     `set -euo pipefail` — `systemctl restart` failing or
     `systemctl status` returning non-zero fails the deploy.

## Manual one-off (no upload)

For migrations, debug commands, or anything you want to fire on a
production host without a build:

```yaml
name: prod-migrate
when:
  event: [manual]                           # only via "Run" button or CLI

jobs:
  run:
    image: alpine:3.20
    uses: gocdnext/ssh@v1
    with:
      host: db.example.com
      user: ops
      key: ${{ secrets.SSH_OPS_KEY }}
      known_hosts: ${{ secrets.DB_KNOWN_HOSTS }}
      script: |
        /opt/migrate/run.sh --env=prod --dry-run=false
```

`event: [manual]` removes the auto-trigger on push — the only way
this fires is from *Run latest* in the dashboard or
`gocdnext run prod-migrate`. Pair with [approval gates](/gocdnext/docs/pipelines/quickstart/)
when the operation is destructive.

## Multiple hosts (fanout)

For a small fleet, declare the hosts in `with.host` and let the
matrix do the work — gocdnext's parser supports per-job matrix:

```yaml
deploy:
  stage: ship
  uses: gocdnext/ssh@v1
  matrix:
    host:
      - api-1.example.com
      - api-2.example.com
      - api-3.example.com
  with:
    host: ${{ matrix.host }}
    user: deploy
    key: ${{ secrets.SSH_DEPLOY_KEY }}
    known_hosts: ${{ secrets.SSH_KNOWN_HOSTS_FLEET }}
    upload: dist/api-server
    target: /opt/api/
    script: |
      sudo systemctl restart api
```

Three parallel jobs, three deploys. A single host failure doesn't
hold the others; the run aggregates `success` only when every matrix
cell does.

## Security checklist

- **Always** set `known_hosts` (or `host_key`). The `host_key_check: "no"`
  escape hatch logs a `WARNING` in run output and disables MITM
  protection — never use it in a real deploy.
- Prefer keys over `password:`. Password auth via `sshpass` exists
  for legacy hosts but each pipeline run prints `WARN` reminders.
- Rotate the deploy key + `known_hosts` together. The known-hosts
  fingerprint changes when the host's host-keys are regenerated.
- Pin the plugin version (`gocdnext/ssh@v1`, not `gocdnext/ssh@latest`)
  so a plugin breaking change can't sneak into a deploy without
  showing up in a PR.
