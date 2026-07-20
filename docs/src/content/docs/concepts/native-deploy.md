---
title: Native deploys (ArgoCD)
description: "Register a native deploy target for an environment and gocdnext becomes the executor — it drives the ArgoCD sync and watches the Application to Synced + Healthy, server-side, with no agent and no deploy script."
---

By default, [`deploy:`](/gocdnext/docs/concepts/deployments/) is a
**tracking marker**: your job runs its own `script:` / `uses:` to
ship, and gocdnext records what shipped where. A **native deploy
target** flips that for one environment — register it, and gocdnext
becomes the **executor**: it drives the ArgoCD sync and watches the
Application to `Synced + Healthy` itself, **server-side, with no agent
and no deploy `script:`**.

The two are the same `deploy:` block. Whether a job is tracking-only
or native is decided purely by **whether its environment has a
registered target** — the pipeline YAML doesn't change.

## Two behaviours of one `deploy:` job

| Environment has… | Behaviour |
|---|---|
| **no** registered target | **Tracking layer** (the default). The job runs your `script:`/`uses:` on an agent; a [deployment revision](/gocdnext/docs/concepts/deployments/#environments-and-revisions) is recorded from the job's outcome. |
| a **native target** | **Native / server-managed.** No agent, no `script:` — gocdnext issues the ArgoCD sync (in `trigger` mode) and watches the Application to convergence. The job's success **is** the deploy's convergence. |

A job whose environment has no target simply falls through to the
normal agent path, so registering a target is a **non-breaking opt-in
per environment** — nothing else in the pipeline moves.

## What "native" means

gocdnext runs a **server-side deployment provider** — a thin client
over the ArgoCD API. It **observes and syncs, but never reconciles**:
ArgoCD stays the reconciler and the manifest renderer
(helm/kustomize/your GitOps repo). gocdnext does not become a second
controller — it asks ArgoCD to sync, then watches the Application's
`.status` to a verdict.

> **Scope today:** the provider implements **sync + watch** (observe an
> Application to `Synced + Healthy`, or trigger the sync first). Argo
> Rollouts control (`Promote`/`Abort`, gate-driven canary) and a
> `git-only` write mode are described in
> [ADR-0001](https://github.com/klinux/gocdnext/blob/main/adr/0001-native-argocd-rollouts-deployment-provider.md)
> as future phases and are **not available yet** — don't rely on them.

## Registering a target

Registration is **API-only today** (a maintainer UI dialog is not yet
shipped) and **maintainer-gated** — list, upsert, and delete all
require the `maintainer` role, because the target reveals which
cluster / Application / namespace an environment deploys to.

```bash
# Upsert the target for one environment (1:1, keyed on environment).
curl -sS -X POST \
  -H "Authorization: Bearer $GOCDNEXT_TOKEN" \
  -H "Content-Type: application/json" \
  https://gocdnext.example.com/api/v1/projects/shop/deploy-targets \
  -d '{
    "environment": "production",
    "cluster":     "argocd-hub",
    "application": "shop-prod",
    "namespace":   "argocd",
    "sync_mode":   "trigger"
  }'
```

| Field | Required | Notes |
|---|---|---|
| `environment` | **yes** | The gocdnext environment this target deploys — the **match key** for a `deploy:` job. Must match `^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`. |
| `cluster` | **yes** | A [registered cluster](/gocdnext/docs/concepts/clusters/) name — **the cluster whose API hosts the ArgoCD Application CR** (the ArgoCD hub, see below), *not* the workload's destination cluster. |
| `application` | **yes** | The ArgoCD `Application` name. |
| `namespace` | no | Namespace holding the Application CR. Defaults to `argocd`. |
| `sync_mode` | **yes** | `trigger` or `observe` (no default). |

`provider` is always `argocd` — you don't send it.

**Validation is fail-closed and happens before any write.** Beyond the
field checks, the server **fetches the real Application** through the
cluster to confirm it: (a) exists and is reachable, (b) the project is
authorised for that cluster, and (c) is **single-source** —
multi-source Applications are rejected (`422`). A registration that
passes has already proven the whole read path works.

```bash
# List the project's targets
curl -sS -H "Authorization: Bearer $GOCDNEXT_TOKEN" \
  https://gocdnext.example.com/api/v1/projects/shop/deploy-targets

# Remove one (frees the environment back to tracking-only)
curl -sS -X DELETE -H "Authorization: Bearer $GOCDNEXT_TOKEN" \
  https://gocdnext.example.com/api/v1/projects/shop/deploy-targets/production
```

Every upsert/delete is audited (`deploy_target.set` /
`deploy_target.delete`).

## `trigger` vs `observe`

| Mode | For | What gocdnext does |
|---|---|---|
| `trigger` | **manual-sync** Applications | Issues the sync itself — patches the Application's `.operation` (as user `gocdnext`, carrying the app's own `syncPolicy.syncOptions` such as `CreateNamespace=true`) — then watches to convergence. This is the mode where a gocdnext gate can precede the sync. |
| `observe` | **auto-sync** Applications | Issues **no** sync. An external GitOps writer commits and ArgoCD auto-syncs; gocdnext only **watches** the Application to `Synced + Healthy`. |

In both modes the deploy succeeds only when the Application reports
`Synced + Healthy` **at the revision this run expects** (correlated by
the full commit SHA ArgoCD reports). If it doesn't converge within the
deadline (**15 min** default) the deploy fails with a progress-deadline
error; a `Degraded` health is debounced briefly before failing, to ride
out a transient blip.

## End-to-end examples

### Gated deploy in `trigger` mode (build → approve → native sync)

Register the target once (maintainer):

```bash
curl -sS -X POST \
  -H "Authorization: Bearer $GOCDNEXT_TOKEN" \
  -H "Content-Type: application/json" \
  https://gocdnext.example.com/api/v1/projects/shop/deploy-targets \
  -d '{"environment":"production","cluster":"argocd-hub","application":"shop-prod","sync_mode":"trigger"}'
```

Then the pipeline — a gate, then the deploy job:

```yaml
jobs:
  build:
    stage: build
    image: golang:1.23
    script: ["make build"]

  promote-prod:
    stage: deploy
    approval:
      description: "Promote to production"
      approver_groups: [release-approvers]

  ship-prod:
    stage: deploy
    needs: [promote-prod]
    # The body is the FALLBACK — it runs only if `production` has NO
    # registered target. With the target above registered, gocdnext
    # syncs ArgoCD itself and this `uses:` never runs.
    uses: ghcr.io/klinux/gocdnext-plugin-argocd@v1
    with:
      command: "app sync shop-prod"
    deploy:
      environment: production
      # No `version:` → correlates against THIS run's commit
      # (CI_COMMIT_SHA) — the SHA ArgoCD reports as synced.
```

On this run: `build` runs on an agent, `promote-prod` blocks for the
approval, and once approved `ship-prod` is taken over by the server —
**no agent** — which patches the Application's `.operation` to sync
`shop-prod`, then watches it to `Synced + Healthy` at the run's commit.
The gate precedes the sync, which is exactly what `trigger` mode is for.

:::caution[The `deploy:` job always needs a body — it's your fallback]
The parser rejects a `deploy:` job with no `script:` / `uses:` /
`image:+settings:`. That body is **the fallback deploy**: with a native
target registered it never executes (the server syncs instead); remove
the target and the same job degrades to running the body on an agent.
So you can adopt — or back out of — native deploys **without touching
the pipeline**. A natural body is the [`argocd` plugin](/gocdnext/docs/reference/plugins/#argocd)
(as above) or your existing `kubectl`/`helm` step.
:::

:::caution[`version:` must be a git SHA for native deploys]
A native deploy correlates success against the **full git commit SHA
ArgoCD reports** as synced. So `version:` must be a commit SHA — or
omitted, which uses the run's own commit (`CI_COMMIT_SHA`), the common
case. An **image tag or semver** (e.g. `${{ needs.build.outputs.image-tag }}`,
fine for a [tracking-layer deploy](/gocdnext/docs/concepts/deployments/))
**fails a native deploy terminally** — it can't be tied to a commit. If
your manifests live in a separate GitOps repo at a different SHA, pin
`version:` to that commit's full/short SHA.
:::

### Watch-only in `observe` mode (auto-sync app)

For an Application that ArgoCD already **auto-syncs** (an external
GitOps writer commits the manifests), register the target as `observe`:

```bash
curl -sS -X POST \
  -H "Authorization: Bearer $GOCDNEXT_TOKEN" -H "Content-Type: application/json" \
  https://gocdnext.example.com/api/v1/projects/shop/deploy-targets \
  -d '{"environment":"staging","cluster":"argocd-hub","application":"shop-staging","sync_mode":"observe"}'
```

```yaml
jobs:
  watch-staging:
    stage: deploy
    uses: ghcr.io/klinux/gocdnext-plugin-argocd@v1   # fallback only
    with:
      command: "app wait shop-staging --health"
    deploy:
      environment: staging
```

gocdnext issues **no** sync here — it just watches `shop-staging` to
`Synced + Healthy` at the run's commit and finalizes the job on
convergence (or fails on the deadline). Use `observe` when something
else owns the sync and you only want gocdnext to gate the pipeline on
the real rollout landing.

## Centralized ArgoCD: `cluster:` is the hub, not the destination

This is the part worth being explicit about. In the target,
**`cluster:` is the cluster where ArgoCD runs** — the one whose
Kubernetes API serves the `Application` CR that gocdnext reads and
patches. It is **not** the cluster the workload lands on.

Where the workload lands is the Application's own `spec.destination`,
which is **entirely ArgoCD's concern** — gocdnext never reads or writes
it. So a **single centralized ArgoCD managing many destination
clusters is the natural fit**, and it needs no special handling:

```
                 ┌────────────────────── ArgoCD hub cluster ──────────────────────┐
  gocdnext ────► │  ns: argocd                                                     │
  (reads +       │    Application "shop-staging"  → spec.destination: cluster-a    │ ──► workload → cluster-a
   patches       │    Application "shop-prod"     → spec.destination: cluster-b    │ ──► workload → cluster-b
   the CR)       │    Application "shop-eu"       → spec.destination: cluster-eu   │ ──► workload → cluster-eu
                 └─────────────────────────────────────────────────────────────────┘
```

| Topology | How to register |
|---|---|
| **Centralized ArgoCD** (one hub, many destination clusters) | Register the **hub** once in the [cluster registry](/gocdnext/docs/concepts/clusters/). Every target points `cluster:` at that hub, `namespace: argocd`, and names a different `application`. |
| **ArgoCD per environment/region** (several hubs) | Register each hub as its own cluster; each target's `cluster:` points at the right hub. |

**Credentials.** The hub cluster is a normal
[cluster-registry](/gocdnext/docs/concepts/clusters/) entry —
`kubeconfig` or a scoped `token`. Because the control plane reaches the
hub *from outside*, **`in_cluster` credentials are rejected** for
native targets (they're only valid from inside that cluster's own
pods). The token needs least-privilege RBAC on the Application CRs — at
minimum `get` (to observe) and `patch` (to sync in `trigger` mode):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  namespace: argocd
  name: gocdnext-deployer
rules:
  - apiGroups: ["argoproj.io"]
    resources: ["applications"]
    verbs: ["get", "list", "watch", "patch", "update"]
```

## What the operator sees

The web UI is **read-only** for native targets (registration is the API
above):

- **Environments card** — a native row per environment: *Native ·
  ArgoCD · app `<application>` · cluster `<cluster>` · `<sync_mode>`*
  (an eye icon for `observe`, a refresh icon for `trigger`). The config
  detail is maintainer-only.
- **Live watch chip** — while a native deploy is in flight, a chip
  shows `Deploying` → `Syncing` (once the sync is requested) →
  `Degraded <time>` if health drops. Backed by
  `GET /api/v1/projects/{slug}/deploy-watches` (viewer-readable, but
  config fields are maintainer-only).
- **Server logs** — the watch loop emits `watch_claimed`,
  `watch_observed`, `watch_decision`, and `watch_finalize`; the
  scheduler logs `deploy_native_selected` / `native deploy dispatched`
  when a job takes the native path (never dispatched to an agent).

## Fail-closed posture

Native deploys never fake success. An unreachable hub, an auth error, a
revision mismatch, or a non-convergent Application all **hold or fail**
— they never mark an environment healthy on incomplete evidence. A
target-resolution error (as opposed to "no target registered") stops
the dispatch and retries next tick rather than silently falling back to
an agent run.

## What this is not (yet)

- **Not a reconciler.** gocdnext asks ArgoCD to sync and watches the
  result; ArgoCD owns desired state and rendering.
- **Not rollout control yet.** Argo Rollouts promote/abort and
  gate-driven canary are a future phase (ADR-0001), not shipped.
- **Not a replacement for the [`argocd` plugin](/gocdnext/docs/reference/plugins/#argocd).**
  The fire-and-forget plugin remains for bespoke setups; a pipeline
  opts into native `deploy:` when it wants convergence + gate coupling.

## See also

- [Deployments & rollback](/gocdnext/docs/concepts/deployments/) — the tracking-layer default and one-click rollback
- [Cluster registry](/gocdnext/docs/concepts/clusters/) — how the hub cluster's credential is stored and resolved
- [Approval gates](/gocdnext/docs/concepts/approvals/) — the gate that precedes a `trigger`-mode sync
