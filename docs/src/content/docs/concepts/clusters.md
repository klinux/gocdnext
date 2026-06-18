---
title: Cluster registry
description: "Register a Kubernetes deploy-target cluster once (encrypted, RBAC, audited); a job references it by name with `cluster:` and the scheduler injects its kubeconfig as PLUGIN_KUBECONFIG at dispatch — masked — so kubectl/helm/kustomize jobs authenticate without a pasted kubeconfig secret."
---

A **cluster** is a registered Kubernetes deploy target. An admin
registers it once — name, auth, governance — and any pipeline job
references it by name:

```yaml
jobs:
  deploy:
    stage: ship
    uses: ghcr.io/klinux/gocdnext-plugin-kubectl@v1
    with:
      command: "apply -k k8s/"
    cluster: prod-gke
```

At dispatch the scheduler resolves `prod-gke` to its stored
kubeconfig and injects it as `PLUGIN_KUBECONFIG` (masked in the log
stream). The kubectl / helm / kustomize plugins already read that
env, so the job authenticates to the cluster with **no pasted
kubeconfig in the pipeline YAML and no `*_KUBECONFIG_B64` secret per
project**.

## Why — the kubeconfig-in-the-step antipattern

The classic GoCD shape is to paste a base64 kubeconfig into every
deploy step (or carry it as a per-pipeline secret) and resolve it
with `with.kubeconfig: ${{ PROD_KUBECONFIG_B64 }}`. That means the
same credential is duplicated across every pipeline that ships to
the cluster, rotation is a fan-out edit, and there's no single place
that answers "who can deploy where".

The cluster registry centralises it: the credential lives **once**,
encrypted at rest, behind admin-only registration, with a
per-cluster project allow-list and an audit trail. Pipelines name a
target; they never hold a target's credential.

This is a credential-injection layer, not an executor. gocdnext
still doesn't `kubectl apply` for you — your job (or the kubectl
plugin) owns the command. The registry only answers "what
kubeconfig does `prod-gke` mean, and is *this* project allowed to
use it".

## The three auth types

Every cluster has an auth type chosen at registration. All three
end the same way at dispatch — the job sees a working kubeconfig in
`PLUGIN_KUBECONFIG` — but the stored shape differs.

### `kubeconfig` — a full kubeconfig

Store a complete kubeconfig YAML. Injected verbatim. Use this when
you already have a static kubeconfig (a service-account one, see
[the exec-auth caveat](#exec-auth-kubeconfigs-not-supported-yet)).

```yaml
# register (admin → Settings → Clusters → New cluster)
name: prod-gke
auth_type: kubeconfig
kubeconfig: |
  apiVersion: v1
  kind: Config
  clusters:
    - name: prod
      cluster:
        server: https://34.0.0.1
        certificate-authority-data: <base64 CA>
  users:
    - name: deployer
      user:
        token: <static SA token>
  contexts:
    - name: prod
      context: { cluster: prod, user: deployer }
  current-context: prod
allowed_projects: [acme-platform]
```

### `token` — bearer token + API server + CA

Store a service-account bearer token, the API server URL, and the
cluster CA. gocdnext **synthesises** a kubeconfig from the three at
dispatch and injects that. Less to paste than a full kubeconfig
when all you have is a token from a ServiceAccount.

The CA is **required** — gocdnext refuses to synthesise a kubeconfig
with `insecure-skip-tls-verify`, so a token cluster always verifies
TLS against a pinned CA. (Need an insecure dev target? Use a full
`kubeconfig` with the flag set explicitly — that choice is then visible
and yours, not a silent fallback.) The CA cert is a public certificate,
so the registry echoes it back on edit and prefills the form; the
bearer token, like every credential, is write-only and never returned.

```yaml
name: staging-eks
auth_type: token
api_server: https://A1B2.gr7.us-east-1.eks.amazonaws.com
ca_cert: |
  -----BEGIN CERTIFICATE-----
  ...
  -----END CERTIFICATE-----
token: <service-account bearer token>
allowed_projects: []   # empty = any project may target it
```

### `in_cluster` — the agent's own ServiceAccount

Store **no credential at all**. The job pod runs with the agent
namespace's mounted ServiceAccount, and the kubeconfig is the
in-cluster one Kubernetes provides at
`/var/run/secrets/kubernetes.io/serviceaccount`. This only works on
the [Kubernetes isolated runtime](/gocdnext/docs/concepts/kubernetes-runtime/)
where the job runs as a pod in your cluster.

```yaml
name: in-cluster
auth_type: in_cluster
# no kubeconfig, no token, no ca_cert — nothing to store or rotate
allowed_projects: [acme-platform]
```

Because there's no stored secret, there's nothing to leak and
nothing to rotate — authorization is pure Kubernetes RBAC on the
agent namespace SA. The trade-off: the agent can only deploy to the
cluster it *runs in*, and you grant that SA deploy permissions (see
[Setting up an in-cluster ServiceAccount](#setting-up-an-in-cluster-serviceaccount)).

## RBAC + `allowed_projects` governance

Two layers gate who can use a cluster:

- **Admin-only to register.** Creating, editing, or deleting a
  cluster is an admin action (maintainer/viewer can't). Every
  mutation writes an `audit_events` row — who registered/rotated/
  deleted which cluster, when.
- **Per-cluster `allowed_projects` allow-list.** A list of project
  **IDs** permitted to reference the cluster. You pick projects **by
  name** in the registration form; gocdnext stores their IDs (the
  examples below use readable slugs only for illustration — the actual
  stored values are UUIDs). **Empty = any project** may target it (a
  deliberate "shared cluster" shortcut; tighten it for production
  targets). A job in a project not on the list fails loud at dispatch —
  the error names the cluster, never its credential.

```yaml
name: prod-gke
auth_type: kubeconfig
allowed_projects: [acme-platform, acme-payments]   # only these two
```

Existence is validated at **apply** time: `cluster: prod-gke` on a
job referencing a cluster that isn't registered fails the apply with
a message naming `prod-gke`, so a typo surfaces when you push the
pipeline, not at 3 a.m. on a deploy. Authorization (the
`allowed_projects` check) is enforced at **dispatch**, because a
cluster's allow-list can change after a pipeline was applied.

## The kubeconfig is masked in logs

The resolved kubeconfig — full, synthesised, or in-cluster token —
is added to the job's `LogMasks` in the same step it's injected as
`PLUGIN_KUBECONFIG`. If a plugin or `script:` ever echoes the
config or the token, the log stream shows the mask, not the
credential. (Same discipline as [secrets](/gocdnext/docs/concepts/secrets/):
the value enters the mask list the moment it enters the
environment.)

The synthesised-`token` path masks the bearer token too, not just
the assembled kubeconfig — a `kubectl config view` that prints the
token still redacts.

## Setting up an in-cluster ServiceAccount

`in_cluster` mode delegates authorization entirely to Kubernetes
RBAC on the agent namespace's ServiceAccount. The agent's SA already
has the [runtime RBAC it needs to spawn job pods](/gocdnext/docs/concepts/kubernetes-runtime/#rbac);
**deploy** RBAC is separate and the operator grants it explicitly —
gocdnext does not widen the agent SA for you.

Grant the agent namespace SA permission on whatever the deploy job
applies. For a kustomize/kubectl apply into an app namespace:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: gocdnext-deployer
  namespace: acme-app          # the namespace the job deploys INTO
rules:
  - apiGroups: ["apps"]
    resources: ["deployments", "statefulsets", "daemonsets"]
    verbs: ["get", "list", "create", "update", "patch"]
  - apiGroups: [""]
    resources: ["services", "configmaps", "secrets"]
    verbs: ["get", "list", "create", "update", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: gocdnext-deployer
  namespace: acme-app
subjects:
  - kind: ServiceAccount
    name: gocdnext-agent           # the agent SA
    namespace: gocdnext            # the agent's own namespace
roleRef:
  kind: Role
  name: gocdnext-deployer
  apiGroup: rbac.authorization.k8s.io
```

Scope the verbs to what the pipeline actually applies — a kustomize
deploy that only touches Deployments and Services doesn't need
cluster-wide `*`. If the deploy spans namespaces, use a
`ClusterRole` + `ClusterRoleBinding` instead, but keep the rules
tight.

## Example: a kustomize deploy pipeline

A single deploy job that renders and applies a kustomization against
the registered `prod-gke` cluster. No kubeconfig anywhere in the
pipeline — `cluster:` injects it:

```yaml
name: deploy

stages: [build, ship]

jobs:
  build:
    stage: build
    image: alpine
    script: ["./build.sh"]

  deploy-prod:
    stage: ship
    needs: [build]
    uses: ghcr.io/klinux/gocdnext-plugin-kubectl@v1
    with:
      command: "apply -k k8s/"
    cluster: prod-gke
```

Pair it with an [approval gate](/gocdnext/docs/concepts/approvals/)
upstream and a [`deploy:` marker](/gocdnext/docs/concepts/deployments/)
on the apply job to get gating + environment tracking on the same
step.

## Migrating from a kubeconfig secret

If you ship today via a per-project secret and
`with.kubeconfig: ${{ PROD_KUBECONFIG_B64 }}`:

1. Admin registers the cluster once (*Settings → Clusters*), pasting
   the kubeconfig that secret held — pick `kubeconfig`, or `token`
   if all you have is a SA token + API server + CA.
2. Set `allowed_projects` to the projects that ship to it (leave
   empty only for a genuinely shared target).
3. In each pipeline, drop `with.kubeconfig:` and the `secrets:` entry
   for the kubeconfig, and add `cluster: <name>` on the deploy job.
4. Delete the now-unused per-project secret.

`cluster:` is the **single source** of the kubeconfig on a job. The
parser rejects a job that also pastes its own kubeconfig
(`with.kubeconfig:`) or otherwise defines `PLUGIN_KUBECONFIG` (via
`variables:`, `secrets:`, `id_tokens:`, or a `parallel.matrix`
dimension) — so step 3 is a clean swap, not an additive one, and no
second source can silently win and point the deploy at the wrong
cluster. An [approval gate](/gocdnext/docs/concepts/approvals/) can't
declare `cluster:` either (a gate dispatches nothing; put the deploy on
a separate job that `needs:` the gate).

Rotation afterward is a single edit on the cluster record instead of a
fan-out across every pipeline. The credential and CA rotate freely; the
**name is immutable** (it's how every `cluster:` reference resolves at
dispatch) — to rename, delete and recreate, and the delete-guard will
surface any pipeline still pointing at the old name.

## exec-auth kubeconfigs not supported yet

A kubeconfig whose user block runs an external binary for credentials
— `exec:` plugins like `gke-gcloud-auth-plugin` (GKE) or
`aws-iam-authenticator` / `aws eks get-token` (EKS) — is **not
supported** and is **rejected at registration**: the auth helpers
aren't shipped in the job image, and an `exec` block can hide secrets
in argv/env where the log masker can't reach them, so gocdnext refuses
it up front rather than letting it fail opaquely at deploy. `in_cluster`
mode uses the mounted SA, not an exec plugin.

Use a **static-token ServiceAccount kubeconfig** instead: create a
ServiceAccount in the target cluster, mint a (long-lived or
periodically rotated) token for it, and register that token —
`token` auth type, or a full kubeconfig whose user block carries the
`token:` directly. For keyless cloud auth on the *build* side,
[OIDC id_tokens](/gocdnext/docs/concepts/id-tokens/) remain the path;
the cluster registry is specifically for the kubeconfig a deploy job
hands to kubectl/helm/kustomize.

## See also

- [`cluster:` in the YAML reference](/gocdnext/docs/pipelines/yaml-reference/#cluster-target-cluster)
- [Kubernetes runtime](/gocdnext/docs/concepts/kubernetes-runtime/) — the isolated pod model `in_cluster` rides on, and the agent SA's runtime RBAC
- [Secrets](/gocdnext/docs/concepts/secrets/) — the masking discipline `cluster:` mirrors
- [kubectl / kustomize / helm plugins](/gocdnext/docs/reference/plugins/#kubectl) — what consumes `PLUGIN_KUBECONFIG`
