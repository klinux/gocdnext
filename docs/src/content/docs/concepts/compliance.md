---
title: Compliance pipelines
description: Framework-scoped, enforced pipeline policies — mandatory jobs and gates merged into every targeted project that repo authors can't remove or bypass.
---

Compliance pipelines let an admin define **mandatory** stages, jobs, and
approval gates that run on targeted projects — and that a project's repo
**cannot remove, reorder away, or skip**. Think "every PCI service must run a
security scan and a separation-of-duties gate before deploy, whether or not the
team remembers to add it."

It's modelled on GitLab's compliance pipelines: admins own the policy,
developers own their CI, and the server **merges** the two so enforcement can't
be edited out from the repo side.

## The two halves: frameworks and policies

- A **framework** is an admin-defined label — `PCI`, `SOC2`, `HIPAA`. It carries
  no behaviour on its own; it's the thing you *assign to projects* and *target
  with policies*. Assign frameworks to a project on its **Settings** page
  (admin-only); manage the catalogue under **Admin → Compliance**.
- A **policy** is the actual mandatory pipeline content, authored in the normal
  [pipeline YAML schema](/pipelines/yaml-reference/). A policy **targets**
  either specific frameworks or *all projects* (`applies_to_all`). When a
  project carries a framework a policy targets, that policy's jobs are merged
  into the project's pipelines.

A project is **governed** when at least one enabled policy applies to it —
globally, or via a framework it carries.

## How the merge works

Every pipeline row stores **two** definitions:

- `definition_raw` — the pipeline exactly as parsed from the repo YAML.
- `definition` — the **effective** definition after policies are merged. This is
  what materialisation and dispatch read, so there is **zero per-run overhead** —
  the merge happens once, at apply time, and is re-materialised whenever a
  policy, framework, or assignment changes.

The merge is deterministic: policies apply in `priority` order (ascending, ties
broken by name).

### Modes: `inject` and `override`

- **`inject`** (default) appends the policy's stages and jobs to the repo's. By
  default new stages are **prepended** (they run first — a scan gate before the
  build). Set a policy's `position_before` or `position_after` (mutually
  exclusive) to anchor injected stages relative to an existing repo stage.
- **`override`** replaces the repo's stages and jobs entirely with the policy's.
  Use it for a fully server-owned pipeline where the repo has no say.

### The reserved `_compliance_` namespace

Every stage and job a policy contributes **must** be named with the reserved
`_compliance_` prefix (the policy editor rejects anything else). Repo YAML, in
turn, **may not** use that prefix. This makes injected entries:

- **impossible to shadow** — a repo can't define a `_compliance_scan` job to
  pre-empt the policy's;
- **impossible to remove** — they live in a namespace the repo can't touch;
- **clearly attributable** — the UI badges `_compliance_`-prefixed jobs as
  *enforced*, and `gocdnext` flags them in the run view.

Two policies may not contribute the same `_compliance_*` name (they'd produce
duplicate jobs) — the editor surfaces the collision.

## Enforcement guarantees

A mandatory job is only mandatory if it can't be skipped. Compliance closes the
usual bypasses:

- **Non-suppressible trigger.** Every governed repo pipeline gets a
  compliance-owned **default-branch push** material with *no path or event
  filter*, so a repo's `when.paths` / `when.event` can't stop enforcement from
  firing. Existing material on the same repo+branch is *merged*, not replaced —
  its credentials, poll interval, and extra events (tag, pull_request) survive;
  only path/branch narrowing of the compliance push is dropped.
- **No-CI projects still run.** A governed project that ships **no pipeline of
  its own** gets a server-owned **synthetic `_compliance` pipeline** (the policy
  jobs as its definition). So policies run even on a repo with no `.gocdnext`
  config. It's marked *server-managed* in the pipeline list.
- **`[skip ci]` is ignored.** The webhook refuses to honour `[skip ci]` on a
  governed project — you can't comment your way out of a mandatory scan.
- **An SCM source is required.** Enforcement is push-triggered, so it needs a
  registered repo binding the webhook can match. Governing a project with no
  SCM source is **refused** (the operation rolls back) rather than registering
  toothless governance.
- **Separation of duties.** Policy and framework management is **admin-only**
  and fully audited; the developers who own the repo can't author or weaken the
  policies that govern them.

## Authoring a policy

A policy is **metadata** (targeting, mode, priority, positioning — set in the
policy form or API) plus a **`config_yaml`** body in the pipeline schema. A
minimal mandatory-scan policy:

```yaml
stages: [_compliance_scan]
jobs:
  _compliance_scan:
    stage: _compliance_scan
    image: aquasec/trivy:latest
    script:
      - trivy fs --exit-code 1 .
```

Mandatory approval gate (separation of duties before any deploy) — gates merge
the same way jobs do:

```yaml
stages: [_compliance_signoff]
jobs:
  _compliance_signoff:
    stage: _compliance_signoff
    approval:
      description: "Compliance sign-off"
      approver_groups: [security-leads]
      required: 1
```

See [Approval gates](/concepts/approvals/) for group and quorum semantics.

## Previewing the effective pipeline

Because the effective definition is materialised, an admin can **see exactly
what compliance adds without running anything**. On a project's **Settings**
page, the *Effective pipeline preview* card shows each pipeline's merged
definition with policy-injected stages/jobs badged *enforced* and the synthetic
pipeline marked *server-managed*. Toggle **What-if** to preview a hypothetical
framework set (e.g. "what would assigning PCI add?") before committing the
assignment — nothing is persisted.

## How-to: enforce a mandatory scan on all PCI projects

1. **Create the framework.** Admin → Compliance → *Frameworks* → **New
   framework** → name it `PCI`.
2. **Author the policy.** Admin → Compliance → *Policies* → **New policy**:
   - Mode `inject`, target the `PCI` framework.
   - `config_yaml`:
     ```yaml
     stages: [_compliance_scan]
     jobs:
       _compliance_scan:
         stage: _compliance_scan
         image: aquasec/trivy:latest
         script:
           - trivy fs --exit-code 1 .
     ```
   - Leave `position_*` empty to run the scan **first** (prepended).
3. **Assign the framework.** On each PCI-relevant project's **Settings** page,
   tick `PCI` under *Compliance frameworks* and save. (The project needs a
   registered SCM source.)
4. **Verify.** Open the project's Settings preview — every pipeline now shows a
   `_compliance_scan` job badged *enforced*. Projects with no CI of their own
   show a *server-managed* `_compliance` pipeline that runs the scan on every
   default-branch push.

To enforce on **every** project regardless of framework, set the policy's
*applies to all* flag instead of targeting a framework.

## Notes & limits

- Un-governing a project (removing the last applicable policy/framework) tears
  down the synthetic pipeline immediately; compliance-owned **triggers** on a
  repo pipeline revert on the project's next repo sync.
- For very large `applies_to_all` fleets the recompute is synchronous under an
  advisory lock — fine for normal sizes; a batched/generation-based recompute
  is tracked for large installs. Run dispatch never takes the lock, so CI keeps
  flowing during a recompute.
