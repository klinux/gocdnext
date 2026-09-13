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
│   └── housekeeper: alpine / gocdnext-housekeeper
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

## Cache & artifact compression (gzip / zstd)

In isolated mode both the cache tarball AND build artifacts are compressed
**inside the housekeeper sidecar** (`tar` piped through a compressor), and
restored the same way. For large payloads (a populated Gradle/Go cache, or an
artifact that is a big SDK bundle / image tarball) compression, not upload,
dominates the store time — single-threaded gzip caps around 20–25 MB/s.
Worse, gzipping an **already-compressed** artifact (a `.zip`, a `.jar`, an
image layer) burns that CPU for essentially zero size win.

Chart knobs:

- `agent.workspace.housekeeperImage` — point at **`gocdnext-housekeeper`**
  (alpine + `zstd` + pipefail-capable `sh`) to unlock zstd. The default
  `alpine` image has only gzip.
- `agent.cache.compression` — `gzip` (default) or `zstd`. zstd `-T0`
  compresses several times faster and smaller.
- `agent.artifacts.compression` — same, for the artifact store. zstd is
  especially worth it for already-compressed artifacts: its fast path stores
  incompressible blocks near memcpy speed instead of grinding through deflate.
- `agent.workspace.housekeeperCPULimit` — raise (e.g. `"4"`) so zstd `-T0`
  can use multiple cores. The idle CPU **request** stays tiny, so pod
  scheduling is unchanged; the limit only lets the burst happen.

**Restore always auto-detects the codec by the blob's magic bytes** (the same
`UntarGz` path backs both cache and artifact download), so a blob written as
gzip and one written as zstd both restore through the same code. That makes the
switch safe for existing caches/artifacts — but flip `compression: zstd` only
**after** every agent in the fleet runs a version whose housekeeper image can
read zstd (reader-before-writer). Rolling back the codec is instant (set it
back to `gzip`); new stores revert while old zstd blobs still restore.

### Direct upload (`agent.artifacts.directUpload`)

Compression decides *how much* the housekeeper writes; **directUpload decides
where it goes.** By default the isolated-mode artifact upload streams the
tar back through the agent's exec channel (SPDY via the apiserver) and the
agent PUTs it to the store. That exec channel caps around **~20 MB/s**
regardless of disk or network — a multi-GB artifact (an SDK bundle, an image
tarball) burns minutes there even when the object store is seconds away.

With `agent.artifacts.directUpload: true` the **housekeeper curls the
tar+compress stream straight to the store's signed URL** — the bytes go
pod→store at network speed, never through the agent. size + sha256 are still
computed in-pod and cross-checked server-side (`InspectObject`), so integrity
is unchanged. Requirements:

- The housekeeper image has `curl` (the bundled `gocdnext-housekeeper` does).
- The store accepts **chunked PUT** on its signed URL: **GCS does** (it
  doesn't sign `Content-Length`); **S3 does not** — leave it off there.

The agent probes for curl and falls back to the exec-stream path when it's
absent, so enabling it on a rolling fleet never breaks a job. Download is
unaffected (prep already fetches via signed URL). Default off.

> **Artifacts, both paths:** `agent.artifacts.compression: zstd` is safe
> end-to-end. Job→job restore auto-detects the codec, and the manual UI/API
> download is codec-aware too (since v0.106.0) — it names a zstd artifact
> `.tar.zst` and serves `application/zstd`, so a hand-downloaded artifact
> extracts cleanly. Existing gzip artifacts keep downloading as `.tar.gz`.

## Per-profile storage sizing

The workspace PVC size/class above are **agent-global** defaults
(`agent.workspace.size` / `storageClassName`). A single fleet often has
one lightweight majority and a few heavy jobs — a big container-image
build being the classic case. A [runner profile](/gocdnext/docs/concepts/runner-profiles/)
can override storage **per job** so the heavy ones get a bigger/faster
disk without inflating every pod:

| Field | Overrides | Applies to |
|---|---|---|
| `workspace_size` | agent-global workspace PVC size | any isolated job on the profile |
| `workspace_storage_class` | agent-global workspace PVC class | any isolated job on the profile |
| `dind_storage_size` | — (adds a dedicated disk) | **`docker: true`** jobs on the profile |
| `dind_storage_class` | — | **`docker: true`** jobs on the profile |

All four are **optional**. Empty keeps today's behaviour: the agent-global
workspace default, and DinD storing on the node's ephemeral disk.

### Why `dind_storage_*` matters for big images

A `docker: true` job runs a DinD sidecar; dockerd + buildkit keep the image
layer store **and** the export/push staging area under `/var/lib/docker`.
By default that directory lives on the DinD container's writable layer — the
**node's ephemeral disk**. For a multi-GB image, `exporting layers` and
`pushing layers` are dominated by that disk's throughput, not by CPU or the
network. Setting `dind_storage_size` mounts a **dedicated ephemeral PVC** at
`/var/lib/docker`, so the export/push run on a disk you sized for it. On GCE,
PD throughput scales with the provisioned size (a 300Gi `premium-rwo` gives
far more MB/s than a 20Gi one even if you only use a few GB); a `local-ssd`
class is faster still where the node pool provides it.

```yaml
# runner profile (admin UI, or seeded via Helm runnerProfiles[])
- name: image-build
  engine: kubernetes
  tags: [linux, docker]
  workspace_size: 100Gi
  dind_storage_size: 300Gi
  dind_storage_class: premium-rwo
```

```yaml
# pipeline job opts in
build-image:
  agent: { profile: image-build }
  docker: true
  uses: ghcr.io/you/plugin-buildx@v1
  with: { ... }
```

Sizes are validated as positive Kubernetes quantities and classes as
DNS-1123 names at write time, so a typo fails with a clear error instead of
a pod stuck `Pending` on an unbindable PVC. Kubernetes **isolated** mode
only — shared mode and the Shell/Docker engines ignore these fields.

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
