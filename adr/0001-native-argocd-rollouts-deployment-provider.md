# ADR 0001: Native ArgoCD / Argo Rollouts deployment provider

- **Status:** Proposed
- **Date:** 2026-07-04
- **Scope:** `gocdnext` server/agent. External-consumer integration is out of scope
  (a future ADR).

## Context

gocdnext is being positioned as the **delivery control plane**: external systems —
e.g. an IDP that renders manifests and owns the GitOps repository — delegate the
delivery *execution* to gocdnext and consume its state over an API. Today the only
path to ArgoCD is a fire-and-forget plugin (`argocd app sync` as a pipeline job).

A plugin is a **verb**: run one command, get an exit code. Our ArgoCD needs are
**nouns** that carry state over time:

- "this Application, watched until Synced + Healthy",
- "this Rollout, paused at a canary step, released by an approval",
- "sync now, or just observe (auto-sync apps)".

A verb tool fights the grain of a stateful, long-lived, gate-coupled interaction.
Concretely, the plugin causes: every pipeline re-wires it; no real convergence
signal (`exit 0` ≠ healthy); no progressive-delivery control; rollout promotion
needs brittle multi-job polling.

The forces:

- We want a one-line `deploy:` where the *platform* pre-registered *how* an
  environment deploys (matches the existing cluster-registry / secret-backends
  pattern).
- We want convergence + rollout observability in the run / VSM /
  `deployment_revisions`.
- We want **progressive delivery controlled by gocdnext's existing approval gates**.
- ArgoCD must remain the reconciler and manifest renderer — gocdnext must not
  become a second controller.
- The capability must be **API-first**, so external consumers drive it without the
  web UI.

## Decision

Adopt a **first-class, server-side deployment provider** with these commitments:

1. **Native, not a plugin.** A server-side subsystem observes and controls ArgoCD +
   Argo Rollouts. The plugin survives only as an escape hatch (§Consequences).

2. **Observe + control, never reconcile.** The provider is a thin client over the
   ArgoCD API + the Rollouts CRD (`argoproj.io/v1alpha1`). Reconciliation stays with
   ArgoCD; manifest rendering stays with whatever renders it (helm/kustomize/an
   external renderer). This boundary is non-negotiable — crossing it rebuilds a
   worse ArgoCD.

3. **Provider seam.** A `DeploymentProvider` interface with `argocd` as the first
   implementation and room for `argo-rollouts`, `git-only`, and (later) other GitOps
   controllers. ArgoCD is a *provider*, never hardcoded across the codebase.

4. **`deploy:` primitive + platform-owned Deployment Target registry.** The pipeline
   says *which environment*; the platform registered *how* (provider, cluster,
   Application, sync mode, rollout-awareness, governing gate). Consumer-agnostic: the
   registry is populated by whoever, not by any one external system.

5. **Sync *or* observe, per target.** `trigger` (manual-sync apps: gocdnext syncs
   after the gate) vs `observe` (auto-sync apps: gocdnext only watches + controls the
   rollout). A `git-only` mode (gocdnext writes the image tag itself) exists only for
   standalone users with no external GitOps writer.

6. **Gate-driven rollout control — the differentiator.** An Argo Rollout's indefinite
   `pause: {}` step maps to a **gocdnext approval gate**, reusing the existing gate
   machinery (quorum / groups / change-management approval). On approve → `Promote()`;
   on reject / failed analysis → `Abort()` + rollback. The renderer declares the
   rollout *shape*; gocdnext controls the *runtime*; ArgoCD executes.

7. **Server-side, resumable watch.** A deploy is not a job that blocks for the
   rollout's duration; it is a durable, single-claimer watch that survives restarts
   (reusing the claim/lease/replay pattern already built for supersede effects). Per-
   deploy polling in v1; a shared informer only if poll volume later demands it.

8. **API-first.** Registering targets, triggering a deploy, and reading state /
   timeline are all API operations, so an external consumer integrates as a thin
   layer.

## Consequences

**Positive**

- Kills "always wire the plugin": `deploy: { to: <env> }` is the whole pipeline
  surface.
- Real convergence + rollout observability lands in the run, VSM, and revisions.
- Progressive delivery gains the *control* Rollouts lacks, on top of gates we already
  hardened — and **supersede composes for free**: a newer deploy supersedes an
  in-flight paused rollout (latest-wins), and the dispatch backstop already makes a
  stale promote impossible.
- Reuses existing primitives (cluster registry for cluster reach,
  `deployment_revisions` for the ledger, approval gates, durable-effects pattern), so
  it is integration work, not greenfield.
- API-first means an external delivery consumer is a thin layer, not a retrofit.

**Negative / costs**

- A new server-side subsystem with a control loop: polling, reconnection, RBAC, and
  tolerance for Rollouts CRD apiVersion drift — real maintenance surface.
- Couples gocdnext to ArgoCD conceptually; the provider seam limits but does not
  eliminate this.
- A scoped ServiceAccount / token per cluster is required (least privilege: read
  Applications, sync, and get/patch Rollouts) — an operational prerequisite.
- Fail-closed posture everywhere (unreachable ArgoCD, ambiguous analysis) — more
  careful error handling than a plugin's exit code.

**Escape hatch:** the `argocd` plugin remains for bespoke setups and standalone
users. No forced migration; a pipeline opts into `deploy:` when ready.

## Alternatives considered

- **Keep the plugin only.** Rejected: it cannot carry rollout/gate state or a
  resumable watch; the pains above persist.
- **gocdnext as pure CI, delivery owned elsewhere.** Rejected for the control-plane
  goal: the delivery consumer would have to reinvent rollout control + observability
  that gocdnext already has, and gocdnext's deploy machinery would go unused.
- **gocdnext reconciles desired state (ArgoCD image-updater style).** Rejected: it
  breaks git-as-source-of-truth and decouples deploys from CI + gates — the exact
  control we want to keep.

## Follow-up decisions (future ADRs)

Deferred sub-decisions, to be recorded as they are made:

1. **Gate ↔ rollout step:** one gate for the whole rollout vs a gate per `pause`
   step.
2. **Application lifecycle:** does gocdnext create the ArgoCD Application, or assume
   the platform created it (gocdnext only syncs/controls)? Leaning: assume it exists,
   with an optional `ensure`.
3. **Status delivery to consumers:** push (webhook/subscription) vs poll.
4. **Target granularity / ownership:** per-project, per-env, or per-team.
5. **`observe`-mode drift policy:** strictness on an observed-revision mismatch
   (fail-closed vs warn).
6. **Poll → informer threshold:** deploy volume at which per-deploy polling moves to
   a shared watch.

## Phasing

1. Sync + watch (no rollout): `deploy:` + target registry (API-first) + the argocd
   provider syncing/observing to Synced + Healthy, surfaced in the run + revision.
2. Gate-driven rollout control: paused step ↔ approval gate → promote/abort; analysis
   surfacing; supersede composition.
3. `git-only` actuation (helm values / kustomize set-image) + additional providers.
4. External-consumer integration (separate ADR).

## Notes — reference design sketch

Non-normative; details will firm up per slice.

```go
type DeploymentProvider interface {
    Sync(ctx, target, revision) error          // trigger; no-op for observe/auto-sync
    Observe(ctx, target) (DeployState, error)   // one convergence snapshot
    Promote(ctx, target) error                  // rollout runtime control
    Abort(ctx, target) error
}

type DeployState struct {
    Sync        SyncStatus     // Synced | OutOfSync | Unknown
    Health      HealthStatus   // Healthy | Progressing | Degraded | ...
    Rollout     *RolloutState  // nil if not rollout-aware
    ObservedRev string
}
```

```
build → publish image
  → deploy(to: <env>)                 # resolves a DeploymentTarget
    → [git-only only] write image tag       (skipped when an external system commits)
    → pre-sync change-management gate  ── reject ─→ abort
    → Sync()                                 (sync_mode = trigger)
    → watch: Progressing → canary 20% → analysis ✓ → Paused(step)
        → arm approval gate (quorum / change-management)
            ── reject ─→ Abort() + rollback
            ── approve → Promote() → 50% → ✓ → 100% → Healthy
    → record deployment_revision (observed rev, healthy)
```

### Corner cases the implementation must address

- ArgoCD unreachable / auth error → fail-closed, retry with backoff, never mark
  healthy.
- Rollout never converges → convergence timeout → fail + rollback per policy.
- Analysis inconclusive → treat as *not approved*; hold at the gate.
- Concurrent deploys to a lane → supersede applies; backstop blocks a stale promote.
- Server restart mid-watch → resumable claim/lease/replay; never double-promote.
- Application OutOfSync from an external change → detect drift, surface, do not
  promote over it.
- `observe`-mode revision mismatch (another writer raced) → fail-closed on mismatch.
- Rollouts CRD apiVersion drift → tolerate unknown fields; degrade to
  Application-level health if the Rollout can't be parsed.
