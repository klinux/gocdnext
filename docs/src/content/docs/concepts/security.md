---
title: Security dashboard
description: "Per-project vulnerability dashboard built from your scanners' SARIF output — ingestion, severity normalization, cross-run new/fixed tracking, and triage state (dismiss / false-positive / accept) with RBAC and audit."
---

The **Security** tab on every project turns your scanners' raw output into a
living vulnerability list: what's open right now, what's new since the last run,
what got fixed, and what your team has triaged away. There's nothing to wire up
beyond making a scan job emit a **SARIF** artifact — gocdnext does the rest.

It answers the questions a raw scanner log can't: *is this finding new or has it
been here for ten runs? did my last push fix anything? which of these did we
already decide are false positives?*

## How findings get there

Any job that produces a [SARIF](https://sarifweb.azurewebsites.net/) file as an
**artifact** feeds the dashboard. SARIF is the standard output format for
semgrep, trivy, osv-scanner, gitleaks, CodeQL, and most modern scanners.

```yaml title=".gocdnext/security.yaml" {8-10}
jobs:
  trivy-fs:
    stage: scan
    uses: ghcr.io/klinux/gocdnext-plugin-trivy@v1
    with:
      scan-type: fs
      target: .
      format: sarif            # emit SARIF, not the default table/json
      output: trivy.sarif
    artifacts:
      - trivy.sarif            # publish it — the server ingests *.sarif
```

On job completion the server reads every `*.sarif` artifact from the job,
parses it, and stores the normalized findings. Key properties:

- **Parsed on the server, from the artifact.** No agent change, no extra
  upload — it reuses the artifact you already publish. The full SARIF stays your
  retained artifact; the dashboard keeps only normalized fields plus a pointer
  back to the blob for drill-down.
- **A scan only "lands" when it parses cleanly** — including a clean scan with
  zero findings. A scanner job that fails, or whose SARIF is unreadable, never
  overwrites the previous run's known findings. "Scanned clean" and "not scanned
  / scan failed" are different states, and the dashboard never silently drops a
  vulnerability because of a transient error.
- See the [Security scanning recipe](/gocdnext/docs/pipelines/recipes/security-scan/) for
  complete trivy + gitleaks pipelines.

## Severity

SARIF expresses severity inconsistently across tools, so gocdnext normalizes
every finding to **critical / high / medium / low** by resolving, in order:

1. `result.properties.security-severity` (a CVSS score, used by trivy/CodeQL) —
   bucketed ≥9 critical, ≥7 high, ≥4 medium, else low;
2. the same property on the rule;
3. `result.level` (`error` → high, `warning` → medium, `note`/`none` → low);
4. the rule's `defaultConfiguration.level`;
5. the rule's `properties.severity`;
6. low, as a last resort.

The header chips count **open** findings per severity — your actionable backlog.
Accepted risk is shown as its own count, not folded in (see
[Triage state](#triage-state-dismiss--false-positive--accept)).

## The "latest scan" model

The list always reflects the **latest reconciled scan**, tracked independently
per **(pipeline, scanner job, matrix cell)**. That granularity matters:

- A pipeline can run several scanners (trivy *and* semgrep). A clean trivy run
  doesn't hide a semgrep finding whose scan is still in flight or failed in that
  run — each scanner advances on its own.
- A [matrix](/gocdnext/docs/pipelines/yaml-reference/) job (e.g. scanning `linux` and `darwin`
  variants) tracks each cell separately, so one variant going clean doesn't mask
  another's findings.

## Cross-run tracking: new / existing / fixed

Every finding has a stable **fingerprint** (the tool's own, or a hash of
tool + rule + path + line + message). gocdnext keeps a persistent **identity**
per `(pipeline, scanner, matrix, tool, fingerprint)` across runs, which powers:

- **New** — a finding first seen in the current run gets a `New` badge. Quickly
  answers "did this push introduce anything?"
- **Existing** — seen in a prior run too; no badge.
- **Fixed** — an identity that was present before but is **gone from the
  scanner's latest scan** shows under a collapsible *"✓ N fixed since last
  scan"* summary, rendered from a snapshot of its last occurrence (the finding
  itself no longer exists). Fixed is tracked per scanner, so a job that drops a
  tool entirely (e.g. removes semgrep, keeps trivy) correctly retires that
  tool's old findings.

The identity is also where triage state lives — so a dismissal **persists across
runs**: dismiss a finding once and it stays dismissed when it reappears, without
re-triaging every scan.

## Triage state: dismiss / false-positive / accept

Each finding carries a human **state**, set from the per-row menu on the Security
tab:

| State | Meaning | Default visibility |
| --- | --- | --- |
| **Open** | Untriaged / actionable | Shown |
| **Accepted** | Acknowledged risk, intentionally kept | Shown, badged amber |
| **Dismissed** | Won't fix / not relevant | Hidden |
| **False positive** | Scanner is wrong | Hidden |

- **Open and accepted stay visible** by default — accepted risk is a decision you
  want to keep seeing, not silence. **Dismissed and false-positive are hidden**;
  tick **Show resolved** to reveal them.
- State **persists by identity across runs** (a dismissed finding stays dismissed
  when the next scan re-reports it) and is **not touched by re-ingestion** — a
  re-run can never clobber your triage.
- An optional **reason** can be attached; it's shown on the row and recorded in
  the [audit log](/gocdnext/docs/install/auth/). It's free text visible to anyone
  with audit-log access — **don't paste secrets into it**. The UI flags this at
  input time.

### Permissions and audit

- Changing a finding's state requires the **maintainer** role or higher (a
  viewer's attempt is rejected). See [RBAC](/gocdnext/docs/install/auth/).
- Every state change is written to the [audit log](/gocdnext/docs/install/auth/) with the
  actor, the new state, and the reason — so "who dismissed this and why" is
  always answerable.
- **Reading** findings follows the same model as the rest of a run's data
  (coverage, tests, logs): any authenticated user can view them. gocdnext's RBAC
  is global roles, not per-project read ACLs, so there is no cross-project read
  restriction on run-scoped views.

## Filtering

The toolbar filters the list by **severity**, **tool**, and **rule**, and toggles
**Show resolved** to include dismissed / false-positive findings. Filters are
URL-driven, so a filtered view is shareable and survives a refresh.

## API

The dashboard is backed by two endpoints (see the
[API reference](/gocdnext/docs/reference/api/)):

- `GET /api/v1/projects/{slug}/findings` — the findings list with severity
  counts, the fixed set, and per-finding `status` (new/existing) and `state`.
  Query params: `severity`, `tool`, `rule`, `include_resolved`, `limit`,
  `offset`.
- `PUT /api/v1/projects/{slug}/finding-states/{id}/state` — set a finding's
  triage state (`{ "state": "...", "reason": "..." }`). Maintainer+;
  project-scoped.

## Org rollup (Analytics)

The **Analytics** page rolls open vulnerabilities up across every project,
grouped by a [project label](/gocdnext/docs/concepts/analytics/) you choose
(team, tier, domain, …) — the same grouping the DORA and compliance sections use.
Each group shows its open count by severity, with **accepted** risk counted
separately and a clear distinction between a **scanned-clean** group (`0 open`)
and one that's **never been scanned**. Counts are by finding **identity**, not
raw SARIF occurrences, so duplicates don't inflate the numbers.

## Shift-left: PR runs & check runs

On a **pull-request run**, the run page's **Security** tab headlines the findings
**new in this change** — diffed against the latest reconciled scan of the same
scanner series on the PR's **base branch** (open + accepted only; dismissed and
false-positive never count as new). "No comparable base scan" is shown as
distinct from "0 new" so an absent baseline never reads as all-clear, and a
scanner the PR adds (no base to compare) is reported as such rather than inflated
into "new".

The same posture is posted to the **GitHub check run** as a one-line summary
(e.g. `Security — 2 critical, 5 high open · 1 accepted · 3 new vs base`), so a
reviewer sees it without leaving the PR. Because SARIF is ingested asynchronously,
the line **self-heals**: if the scan lands after the run's check already
completed, gocdnext re-patches the check so GitHub converges to the right numbers.

## Not yet

**Gating** a PR on new criticals (a policy decision) and an auto-posted PR
**comment** (the check-run line already gives PR visibility without comment spam)
are intentionally out of scope.
