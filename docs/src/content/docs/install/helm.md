---
title: Helm install
description: Install gocdnext on a Kubernetes cluster via the official Helm chart.
---

The chart at `oci://ghcr.io/klinux/charts/gocdnext` ships everything
the control plane needs: server, agent, web UI, runner profiles
ConfigMap, and an opt-in `postgres-dev` for evaluation. Production
deployments point at an external Postgres via `database.url`.

## Prerequisites

- Kubernetes 1.27+ (any flavour — kind, k3d, EKS, GKE, AKS, on-prem).
- Helm 3.14+.
- An external Postgres 14+ for production. The bundled `postgres-dev`
  is fine for kicking the tyres, never for real workloads.
- An ingress controller if you want the dashboard reachable outside
  the cluster (NGINX, Traefik, anything Kubernetes-native).

## Install

```bash
helm install gocdnext oci://ghcr.io/klinux/charts/gocdnext \
  --version 0.6.4 \
  --namespace gocdnext --create-namespace \
  --set devDatabase.enabled=true \
  --set server.publicBase=http://gocdnext.local
```

This brings up:

- `gocdnext-server` (HTTP :8153, gRPC :8154) — webhooks, REST, agent stream.
- `gocdnext-agent` — pulls jobs and runs them in `docker` (default) or `kubernetes` (set `agent.engine=kubernetes`).
- `gocdnext-web` — the Next.js dashboard.
- `gocdnext-postgres-dev` (only with `devDatabase.enabled=true`).

Watch the rollouts:

```bash
kubectl -n gocdnext get pods --watch
```

## Production: external Postgres

```bash
kubectl -n gocdnext create secret generic gocdnext-db \
  --from-literal=DATABASE_URL='postgres://user:pass@db.internal:5432/gocdnext?sslmode=require'

helm upgrade --install gocdnext oci://ghcr.io/klinux/charts/gocdnext \
  --version 0.6.4 \
  --namespace gocdnext \
  --set database.existingSecret=gocdnext-db \
  --set server.publicBase=https://ci.example.com
```

## Common knobs

The full surface is in `charts/gocdnext/values.yaml`. Most operators
touch only these blocks:

### Auth

```yaml
auth:
  enabled: true
  adminEmails: ["alice@example.com"]
  allowedDomains: ["example.com"]
  google:
    clientID: 123456789.apps.googleusercontent.com
    clientSecret:
      existingSecret: gocdnext-google     # K8s Secret with key AUTH_GOOGLE_CLIENT_SECRET
```

GitHub, Keycloak, generic OIDC follow the same shape under
`auth.github`, `auth.keycloak`, `auth.oidc`.

### Artifacts

```yaml
artifacts:
  backend: s3
  s3:
    bucket: gocdnext-artifacts
    region: us-east-1
    existingSecret: aws-creds            # K8s Secret with AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY
  keepLastPerPipeline: 30
  projectQuotaBytes: "107374182400"      # 100 GiB
```

The backend can also be configured at runtime by an admin via
*Settings → Storage* — values picked there are persisted in the
`platform_settings` table and take precedence over the Helm values
on next server boot. Useful for migrating filesystem → s3 without a
redeploy. Helm values still set the initial state.

### Kubernetes workspace (when `agent.engine=kubernetes`)

```yaml
agent:
  engine: kubernetes
  workspace:
    # ReadWriteOnce (default since v0.5.0): each job pod gets its own
    #   ephemeral PVC provisioned via volume.ephemeral. Works with any
    #   storage class — pd-ssd, local-ssd, anything RWO. Each job is
    #   isolated; jobs do NOT share a workspace.
    # ReadWriteMany (legacy): the agent StatefulSet owns one PVC that
    #   every job pod mounts. Requires an RWX storage class (NFS,
    #   Filestore). Pre-v0.5.0 behaviour, kept for upgrades that can't
    #   migrate yet.
    accessMode: ReadWriteOnce
    storageClassName: ""                 # empty = cluster default
    size: 20Gi
```

The trade-off — and how the pod is composed in each mode — is in
[Kubernetes runtime](/gocdnext/docs/concepts/kubernetes-runtime/).

### Logs

```yaml
logs:
  retention: "720h"                      # drop partitions older than 30 days
  archive:
    policy: auto                         # on iff artefact backend wired
    cacheBytes: "268435456"              # 256 MiB LRU for decoded archives
```

### Plugins

```yaml
extraPlugins:
  myorg-deploy: |
    name: myorg-deploy
    category: deploy
    description: Roll out a service via our internal CD API.
    inputs:
      service: { required: true, description: service slug }
      env: { required: true, description: target environment }
```

The official catalogue is baked into the server image, so out of the
box `with:` validation works for every `gocdnext/*` plugin without
extra configuration.

## Upgrade

```bash
helm upgrade gocdnext oci://ghcr.io/klinux/charts/gocdnext \
  --version 0.6.4 \
  --namespace gocdnext \
  --reuse-values
```

Migrations run goose-forward automatically on server startup. The
full upgrade runbook (with the v0.5.0 workspace-default break and
how to keep the legacy `ReadWriteMany` behaviour) is in
[Upgrade runbook](/gocdnext/docs/install/upgrade/).

## Uninstall

```bash
helm uninstall gocdnext -n gocdnext
kubectl delete namespace gocdnext
```

If you used `devDatabase.enabled=true`, this also drops the bundled
Postgres PVC. External Postgres deployments keep the database
intact — drop it manually if you want a clean slate.
