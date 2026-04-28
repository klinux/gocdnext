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
  --version 0.2.0 \
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
  --version 0.2.0 \
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
  --version 0.2.0 \
  --namespace gocdnext \
  --reuse-values
```

Migrations run automatically on server startup via goose. The 0.2.0
release adds `00027_log_lines_partition` and `00028_log_archive`, both
backward-compatible.

## Uninstall

```bash
helm uninstall gocdnext -n gocdnext
kubectl delete namespace gocdnext
```

If you used `devDatabase.enabled=true`, this also drops the bundled
Postgres PVC. External Postgres deployments keep the database
intact — drop it manually if you want a clean slate.
