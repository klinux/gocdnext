---
title: Kubernetes runtime
description: How agents run jobs on Kubernetes — shared vs isolated workspaces, the init+task+housekeeper pod model, RBAC.
---

When the agent runs with `GOCDNEXT_AGENT_ENGINE=kubernetes` it
launches each job as its own Pod inside the namespace the agent
lives in. There are two ways the workspace under `/workspace` can be
provisioned, controlled by `agent.workspace.accessMode` in the Helm
chart.

## Shared (legacy, `ReadWriteMany`)

The agent StatefulSet owns one PVC (`volumeClaimTemplates`) mounted
into the agent pod. Every job pod the agent spawns mounts the **same**
PVC. Jobs land in their own subdir `/workspace/<run>/<job>` but the
underlying volume is shared.

```
Pod (job-<run>-<job>)
├── volumes:
│   └── workspace: PVC <agent-statefulset-pvc>   (RWX)
└── containers:
    └── task: <user/plugin image>
        volumeMounts: workspace → /workspace
```

Constraints:

- Storage class **must** be RWX-capable: NFS, Filestore, CephFS, etc.
  Performant block storage (`pd-ssd`, `local-ssd`, `gp3`) is RWO-only,
  not usable here.
- Workspace is shared across jobs in the same agent — convenient when
  a downstream job wants the upstream's checkout for free, but a job
  crash can leave stale files behind for the next job.
- Pre-v0.5.0 default; kept for upgrade paths where the operator can't
  migrate off RWX yet.

## Isolated (default since v0.5.0, `ReadWriteOnce`)

Each job pod gets its **own** ephemeral PVC, provisioned via
`volume.ephemeral.volumeClaimTemplate` against the storage class the
operator picked. The PVC is born with the pod and dies with it — no
shared filesystem between jobs.

```
Pod (job-<run>-<job>)
├── volumes:
│   ├── workspace: ephemeral PVC (storageClassName=<cfg>, RWO)
│   └── assignment: Secret (JobAssignment serialised .pb)
├── initContainers:
│   └── prep: gocdnext-agent:<version>
│       command: ["gocdnext-agent", "prep", ...]
│       — clones materials, downloads upstream artefacts via signed
│         URLs, fetches cache tarballs, expands cache key templates
├── containers:
│   ├── task: <user/plugin image>
│   │   command: existing (plugin or user script)
│   └── housekeeper: alpine
│       command: ["sleep", "infinity"]
│       — keeps the pod alive while the agent execs `tar` to stream
│         artefacts + caches out, then the pod is deleted
```

The agent itself does NOT need a workspace PVC of its own in this
mode — the StatefulSet's `volumeClaimTemplates` is conditionally
omitted by the chart.

Trade-offs:

- Works with any storage class. `pd-ssd` / `local-ssd` / `gp3` deliver
  the IO that artefact transfer and cache restore actually need.
- Real isolation: a job crash can never poison the next job's
  workspace; the volume is gone the moment the pod is reaped.
- Slightly slower pod startup (init container does prep work that
  shared-mode did inside the agent process), but the difference is
  drowned by the much faster artefact untar on real block storage.
- Materials are cloned **inside** the prep init container, not in the
  agent. Materials that need network egress need the cluster's egress
  policy to allow it from the job namespace.

## Choosing

| If you have… | Pick |
|---|---|
| Existing v0.4.x install on Filestore/NFS and don't want to migrate yet | `ReadWriteMany` (pin in values, see [Upgrade runbook](/gocdnext/docs/install/upgrade/)) |
| GCP and want speed without Filestore | `ReadWriteOnce` + `storageClassName: pd-ssd` |
| Bare-metal / on-prem with local-path provisioner | `ReadWriteOnce` + `storageClassName: local-path` |
| EKS and want gp3 | `ReadWriteOnce` + `storageClassName: gp3` |
| AKS | `ReadWriteOnce` + `storageClassName: managed-csi` |
| Need cross-job workspace sharing on a single agent | `ReadWriteMany` (no RWO equivalent) |

## RBAC

The agent's ServiceAccount needs the following at the namespace it
runs jobs in:

| Resource | Verbs | Why |
|---|---|---|
| `pods` | `create`, `get`, `list`, `watch`, `delete` | Spawn + reap job pods |
| `pods/log` | `get` | Tail container logs |
| `pods/exec` | `create` | **Isolated mode**: stream `tar` out of housekeeper sidecar for artefact + cache upload |
| `pods/status` | `get` | Detect terminal task container status |
| `secrets` | `create`, `get`, `patch`, `delete` | Materialise the `JobAssignment` for the prep init container; patch owner ref for GC |
| `persistentvolumeclaims` | `list`, `delete` | Cleanup recovery if ephemeral PVCs leak (rare; controller usually reaps them) |

The Helm chart wires this up. `pods/exec` is the one that's specific
to isolated mode; if you've tightened the chart-provided ClusterRole
in your fork, check it's still granted.

## Failure modes

**Init container fails.** Pod ends in `Init:Error`. The agent tails
the prep container logs (`stream=init.prep`) and reports
`JobResult{Status: failed}` with the tail as the failure reason. The
task container never runs.

**Ephemeral PVC provisioning slow.** Pod sits in `Pending` while the
CSI provisioner allocates. Cluster-autoscaler + `WaitForFirstConsumer`
storage class are the usual cause. The job's `start_time` is stamped
when the task container starts, not when the pod is scheduled — so
slow provisioning shows up as queue time in run timing.

**Housekeeper restarts.** Pod has `restartPolicy: Never` in both
modes, so this doesn't happen — if the task container terminates the
pod runs out of work and is cleaned up. The housekeeper only exists
to keep the pod alive for the post-task exec window.

**Secret limit.** `JobAssignment` serialisation includes pre-signed
URLs for artefact downloads and cache fetches. The agent enforces a
~950 KiB cap on the serialised proto; a job that exceeds it (very
large `needs_artifacts` lists or hundreds of cache entries) fails
fast at dispatch with a clear error. Split the work into smaller
jobs if you hit this.

## Migration tips

- Before flipping `ReadWriteMany → ReadWriteOnce` in production,
  validate in a staging namespace with one real pipeline. Artefact
  upload + cache restore are the operations whose timing changes
  most.
- If you depended on workspace state surviving between jobs of the
  same agent (rare — most pipelines use artefacts/caches for this),
  refactor the pipeline to declare the dependency explicitly via
  `needs_artifacts:` or `cache:` before migrating.
- The ephemeral PVC lifetime is the pod's lifetime. If a job pod is
  force-deleted, the PVC goes with it. No risk of leaked PVCs unless
  the CSI controller is wedged — then you'll see them with
  `kubectl get pvc -l gocdnext.io/managed-by=agent`.
