---
title: Runner profiles
description: Named bundles of execution policy — engine, default + max compute, tags, env, secrets, and (v0.14.0+) Kubernetes scheduling hints.
---

A runner profile is a named bundle of execution policy a job
references at apply time. Profiles centralise the knobs that would
otherwise repeat across every YAML in the project — fallback image,
default + maximum CPU/memory, agent tags the job needs, environment
variables (plaintext and secret), and Kubernetes scheduling hints.

Pipelines reference a profile by name:

```yaml
jobs:
  build:
    agent:
      profile: gradle-heavy
    script:
      - ./gradlew build
```

The scheduler resolves the profile at dispatch time, so admins can
update the profile in place (raise the memory cap, add a toleration,
rotate a credential) without re-applying every pipeline that uses
it.

## Engine

Today profiles are scoped to the Kubernetes engine — Shell and
Docker engines accept the YAML field but ignore the profile's
scheduling and resource fields. The `engine: kubernetes` field on
the profile is enforced at apply time.

## Default and maximum resources

`default_*` fills any matching `resources:` slot the job left
empty. `max_*` caps any value the job tries to set higher.

```yaml
# admin profile
name: gradle-heavy
engine: kubernetes
default_cpu_request: 500m
default_cpu_limit: "2"
default_mem_request: 2Gi
default_mem_limit: 4Gi
max_cpu: "4"
max_mem: 8Gi
```

A job referencing this profile without an explicit `resources:`
lands with the defaults; a job that asks for `memory: 16Gi` fails
apply with a clear "exceeds profile max_mem" error.

### Fallback to `default` profile

Since v0.13.1, when a job declares NO `profile:` AND a profile
named `default` exists in the DB, the scheduler auto-applies the
`default` profile's resource bounds (and only the bounds — image,
tags, env, secrets, caps stay strictly opt-in via explicit
`profile: default`). This closes a footgun where a missing profile
reference produced a pod with no `resources:` block, leaving it
OOM-killed by the namespace's LimitRange or unbounded by the node.

## Tags

Tags are how the scheduler routes a job to a compatible agent.
Profile tags merge (union) with job-declared tags; an agent must
carry every tag for the job to dispatch.

```yaml
name: docker-builds
tags: [linux, docker]
```

A job inheriting this profile against an agent declaring
`tags: [linux, docker, gpu]` matches; an agent with only `[linux]`
does not.

## Environment and secrets

`env` is a plain key/value map injected into every container the
profile runs. Useful for non-secret runtime config (bucket names,
regions, registry mirrors).

`secrets` are encrypted at rest with the server's AEAD cipher and
unsealed at dispatch. The API only ever returns secret keys, never
values. Secret values may reference globals via
`{{secret:NAME}}` templates so a value rotated once globally
flows into every profile that references it.

## Scheduling hints (v0.14.0+)

Profiles can pin job pods to specific nodes via `node_selector` and
tolerate specific taints via `tolerations`. Honoured by the
Kubernetes engine only.

### `node_selector`: agent baseline + profile, profile wins

The Kubernetes engine builds the pod's `nodeSelector` by **merging**
two sources:

1. **Agent baseline** — `agent.jobNodeSelector` in the Helm
   `values.yaml` (rendered to the env var
   `GOCDNEXT_K8S_JOB_NODE_SELECTOR` on the agent StatefulSet).
   This is the fleet-wide default applied to every pod the agent
   creates — task pods (shared + isolated modes), the housekeeper
   sidecar, and `services:` sidecar pods.

2. **Profile** — `node_selector` in the runner profile
   (admin UI at `/admin/profiles` or Helm
   `runnerProfiles[].node_selector`).

**On key collision, the profile wins.** Profile is more specific
than the agent default — a job declaring `profile: gradle-heavy`
with `pool: gradle` lands on gradle nodes even when the agent
baseline says `pool: ci`.

### `tolerations`: agent baseline + profile, profile appends

Same two sources for tolerations. **The lists concatenate** with the
agent baseline first; profile entries are appended. Kubelet ignores
exact duplicates so dedup is not needed on the agent side.

### Services inherit ONLY the agent baseline

`services:`-declared sidecar pods (postgres, redis, etc.) receive
the agent baseline `nodeSelector` + `tolerations` so they can
schedule on the same tainted nodes as the task pod. They do **not**
receive profile-scoped scheduling — services attach to the run, not
to a specific job, and don't carry the profile reference at
materialisation time.

If the task pod runs on `pool: gradle` (per the profile) but the
service pod needs to land alongside it, taint the gradle nodes
generously enough that both pass via the agent baseline. The follow-
up where services accept their own profile-scoped scheduling is
tracked separately.

### Chart values

```yaml
agent:
  jobNodeSelector:
    pool: ci
  jobTolerations:
    - key: ci-only
      operator: Equal
      value: "true"
      effect: NoSchedule
```

Empty defaults skip the env var entirely, so the StatefulSet on an
unconfigured chart matches pre-v0.14 behaviour bit-for-bit.

### Validation

Both `node_selector` keys and values validate against the same
rules the Kubernetes apiserver enforces at pod admission
(`IsQualifiedName` / `IsValidLabelValue`), so a misconfig surfaces
as a 400 at admin write time, not as a Pending pod hours later when
the next job dispatches.

Tolerations validate operator (`Equal` / `Exists`), effect
(`NoSchedule` / `PreferNoSchedule` / `NoExecute` / empty),
`Exists`-with-value rejection, and `toleration_seconds` only with
`effect: NoExecute`. Empty `operator` normalises to `Equal`
server-side so persisted rows always carry the explicit form.

## Seed via Helm

Both UI-created and Helm-seeded profiles coexist. The chart's
`runnerProfiles` list upserts each entry by name on boot — profiles
not in the list are left alone (operator-created in the UI), and
profiles in the list both create-if-missing and update-in-place.

```yaml
# values.yaml
runnerProfiles:
  - name: default
    description: Sensible runtime bounds for any pipeline.
    engine: kubernetes
    default_cpu_request: 100m
    default_cpu_limit: "1"
    default_mem_request: 256Mi
    default_mem_limit: 1Gi
    max_cpu: "4"
    max_mem: 8Gi
    tags: [linux]
  - name: gradle-heavy
    engine: kubernetes
    default_cpu_request: 1
    default_cpu_limit: "4"
    default_mem_request: 4Gi
    default_mem_limit: 8Gi
    max_cpu: "8"
    max_mem: 16Gi
    tags: [linux]
    node_selector:
      pool: gradle
    tolerations:
      - key: gradle-only
        operator: Equal
        value: "true"
        effect: NoSchedule
```

Secrets are deliberately **not** seeded from `values.yaml` — a
values file commonly lives in git, plaintext credentials there
are a foot-gun. Manage `secrets:` post-install via the admin UI
(or sealed-secrets that the chart-managed entries can reference
via the `{{secret:NAME}}` template syntax).

## Audit

Every profile create / update / delete is recorded in the audit
log under `audit_events.action` values `runner_profile.create`,
`runner_profile.update`, `runner_profile.delete`. The metadata
captures which fields changed so admins can `kubectl -n gocdnext
exec` into the DB and reconstruct the state at any point.

## See also

- [Approval gates](/gocdnext/docs/concepts/approvals/) — separate
  admin-managed catalogue used at run time.
- [Kubernetes runtime](/gocdnext/docs/concepts/kubernetes-runtime/) —
  how pods are spawned, what the agent baseline applies to.
