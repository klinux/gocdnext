# Changelog

All notable changes to gocdnext.

The format follows [Keep a Changelog](https://keepachangelog.com/),
versions follow [SemVer](https://semver.org/) (with the v0.x.y
convention that minor bumps may carry breaking changes until 1.0).

## v0.14.4 — 2026-06-09

Closes [#15](https://github.com/klinux/gocdnext/issues/15) and the
pre-existing `**` glob gap operators hit on Gradle/Maven layouts.

### Feature — test_reports parity in isolated workspace mode

Before this release, jobs running under
`agent.workspace.accessMode: ReadWriteOnce` (isolated mode, the
default since v0.5.0) emitted a warn line saying JUnit collection
was unsupported and the Tests tab would stay empty. The agent
couldn't walk the pod's ephemeral PVC from outside, so
`test_reports:` globs were silently dropped.

After v0.14.4, isolated mode collects test reports via the same
`PodExecutor.Exec` plumbing the outputs and artifact paths already
use:

1. `find <workDir> -type f` once inside the housekeeper sidecar.
2. Agent-side glob match against each declared YAML pattern.
3. `cat -- <path>` per match → bytes flow back through the SPDY
   stream and into the existing JUnit decoder.
4. Aggregated `TestResultBatch` ships through the same gRPC stream
   shared mode uses — UI's Tests tab renders identically.

Reports are scanned on both success and non-zero-exit paths,
matching shared mode (the Tests tab carries its highest signal
exactly when a build fails).

### Fix — `**` recursive glob now works in BOTH modes

`expandGlobs` previously used `filepath.Glob`, which treats `*` as
"any chars except path separator" — `**/build/test-results/test/*.xml`
silently matched zero files. The replacement
`doublestar.FilepathGlob(..., WithFilesOnly())` understands `**`
as "any number of path segments," lining up with the Gradle / Maven
/ pytest convention every CI tool already supports. Shared-mode
operators who configured `test_reports: ["**/…"]` and saw an empty
Tests tab will now see the reports they expected.

### New dependency

`github.com/bmatcuk/doublestar/v4` (agent only). Stdlib
`filepath.Glob` provides no `**` semantics and no plan to add it;
the canonical Go implementation is small, MIT, and used widely
across the ecosystem. Confined to `agent/internal/runner` so the
server's import surface is unchanged.

## v0.14.3 — 2026-06-09

Observability hotfix. Before this release, when a webhook push
landed but `applyDrift` decided to skip — either because the push
was on a non-default branch, or because the server had no
`ConfigFetcher` wired — the skip was **silent**. Operators
staring at "I pushed to my project's configured default and drift
didn't fire" had no signal whether the branch comparison,
fetcher wiring, or something later in the path was at fault.

### Fix

`server/internal/webhook.Handler.applyDrift` now emits an info
log on every skip:

- branch mismatch → logs `pushed_branch` AND the configured
  `default_branch` side-by-side, so a typo in either is obvious
  (e.g. project default = `gocdnext-tests`, push on `main` →
  the diagnostic surfaces both values without needing DB
  inspection).
- no fetcher wired → logs the missing dependency explicitly.

Regression coverage on the branch-mismatch path asserts both
field names appear in the log; the configured default and the
pushed branch both surface so a grep on `drift skipped` after a
push lands the answer immediately.

## v0.14.2 — 2026-06-08

Hotfix on v0.14.1. Closes a silent gap where ApplyProject from the
Sync handler (UI / CLI re-fetch) and the Drift handler (webhook
push) ran **without** calling `ResolveProfiles` first. Only the
CLI Apply handler did. Result: a job declaring
`agent.profile: foo` had its `resources` left zeroed in the
persisted `pipelines.definition` JSONB even when the `foo` profile
had bounds configured — the scheduler then materialised pods with
no `resources:` block, the kubelet did its own thing, and the
operator chased "why didn't my profile apply?" after editing a
profile and clicking Sync (or after a webhook push triggered a
drift re-apply).

### Fix

`server/internal/api/projects.Handler.Sync` and
`server/internal/webhook.Handler.applyDrift` now call
`store.ResolveProfiles(ctx, parsed)` immediately before
`ApplyProject`, mirroring the CLI apply path. The persisted
definition now carries the resolved bounds (and, where the YAML
omits them, the profile's `node_selector` / `tolerations` too —
same fill-step as Apply).

Regression coverage: integration tests on both handlers seed a
`default` profile with bounds, run the path end-to-end, then
parse the persisted JSONB and assert `Requests` / `Limits` match
the profile — not just that the fetcher was invoked.

The drift handler keeps its default-branch guard intact:
broadening drift to non-default branches is gated on a separate
follow-up (only re-apply when the pushed branch is itself a
registered material for the project), so a feature branch can't
overwrite the project's global definition.

### Fix — Cancel actually kills the pod

Cancelling a running job (server-side CancelJob → Runner.Cancel →
job ctx canceled) no longer leaves the pod alive when the engine
runs with `CleanupOnFailure=false` (the default operators run).
Both the shared-mode `maybeCleanup` and the isolated-mode
`cleanupIsolatedPod` paths now detect `context.Canceled` and
force-delete the pod regardless of cleanup policy, using a fresh
background ctx (bounded by a 10s `cleanupPodDeleteTimeout`) for
the DELETE so the canceled run ctx doesn't abort the call and a
wedged apiserver can't pin the runner on the very path that's
supposed to free the slot. Cleanup policy still applies to
natural failures (non-zero exits, prep crashes) — those keep the
pod for debugging as before.

## v0.14.1 — 2026-06-08

Hotfix on v0.14.0. Closes the inconsistency where a job that
declared no `agent.profile:` inherited the `default` profile's
resource bounds at apply time (the v0.13.1 fallback) but did NOT
inherit its `node_selector` / `tolerations` at dispatch time —
the safety net stopped half-way and pods landed on the wrong
nodes despite the admin configuring `default`'s scheduling.

### Fix

`scheduler.resolveProfile` now falls back to the `default` profile
by name when the job declares no profile, mirroring the
apply-time bounds fallback. Missing `default` profile → no-op
(same behaviour as before the fallback existed). A job that DOES
declare a profile and that profile is missing still fails the
dispatch loud.

Result: jobs without an explicit profile now inherit the
`default`'s NodeSelector + Tolerations in addition to the bounds,
matching the contract operators reasonably expect when they
configured a single `default` profile to handle the fleet.

## v0.14.0 — 2026-06-08

Two related features that close the remaining footguns operators
hit when adopting profile-driven workloads: a `default`-profile
fallback for jobs that declare nothing, and full Kubernetes
scheduling hints (`node_selector` + `tolerations`) on every runner
profile. Together they unblock the canonical Cora pain case —
Gradle multi-module builds that OOM-killed on unbounded pods and
landed Pending when the cluster pinned CI to tainted nodes.

### Feature — fallback to `default` profile bounds

When a job declares no `profile:` AND a profile named `default`
exists in the DB, the scheduler now auto-applies `default`'s
resource bounds at apply time. Only the bounds — image, tags, env,
secrets, caps stay strictly opt-in via explicit `profile: default`.
Closes the "missing profile reference produced unbounded pod →
OOM-killed by the namespace LimitRange" failure mode without
forcing every YAML to grow a profile reference.

Clusters with no `default` profile see no behaviour change — the
fallback is a no-op.

### Feature — `node_selector` + `tolerations` on runner profile

Profiles now carry Kubernetes scheduling hints:

```yaml
# admin UI / Helm runnerProfiles[]
name: gradle-heavy
engine: kubernetes
default_mem_request: 4Gi
default_mem_limit: 8Gi
node_selector:
  pool: gradle
tolerations:
  - key: gradle-only
    operator: Equal
    value: "true"
    effect: NoSchedule
```

A job referencing the profile lands on nodes labelled `pool=gradle`
and tolerates the `gradle-only=true:NoSchedule` taint. Honoured by
the Kubernetes engine only; Shell + Docker engines ignore.

**Merge contract**:

- `node_selector` merges with the agent-level baseline (Helm
  `agent.jobNodeSelector`). Profile values WIN on key collision
  — profile is more specific than agent default.
- `tolerations` concatenate: agent baseline first, profile entries
  appended. Kubelet ignores exact duplicates so dedup is not
  applied.
- Service pods (`services:` sidecars) inherit ONLY the agent
  baseline. Per-service profile-scoped scheduling is a separate
  follow-up.

**Validation** at admin write time uses the same
`k8svalidation.IsQualifiedName` / `IsValidLabelValue` the
apiserver applies at pod admission, so a misconfig surfaces as
HTTP 400 immediately, not as a Pending pod hours later. Toleration
invariants enforced: `Exists+value` rejected, empty operator
normalises to `Equal`, `toleration_seconds` only with
`effect: NoExecute`, key/value follow label rules.

### Chart values for the agent baseline

```yaml
# values.yaml
agent:
  jobNodeSelector:
    pool: ci
  jobTolerations:
    - key: ci-only
      operator: Equal
      value: "true"
      effect: NoSchedule
```

Empty defaults skip the env var entirely; the StatefulSet on an
unconfigured chart matches pre-v0.14 behaviour bit-for-bit.

### Admin UI

`/admin/profiles` editor grows a **Node selector** + **Tolerations**
section. Tolerations editor enforces cross-field invariants
client-side (operator=Exists disables value, effect≠NoExecute
disables `toleration_seconds`) so the form mirrors the server
rules. Profile-edit Sheet width responsive — full viewport on
mobile, 85vw on tablet, 50vw on desktop. Validation is intentionally
permissive client-side; the server returns the canonical k8s error
message.

### Audit + REST

- New audit action `runner_profile.scheduling_updated` is implicit
  in the existing `runner_profile.update` event — the metadata
  field captures the before/after of `node_selector` and
  `tolerations` so admins can reconstruct policy history.
- OpenAPI gains `Toleration` (read) and `TolerationWrite` (write)
  schemas. RunnerProfile + RunnerProfileWrite expose the two new
  fields with `always-present-on-read` semantics (`{}` / `[]`,
  never null).

### Internal hardening

- Tolerations deep-copy `*int64` `TolerationSeconds` at every
  proto↔engine↔store boundary so a future caller cache that reuses
  a slice can't mutate an already-shipped JobAssignment or pod
  spec.
- Service pods now receive the agent-level `Tolerations` baseline.
  Previously documented as "applies to all pods" but only wired
  for task pods — a cluster with NoSchedule taints would have left
  service pods Pending while the task pod scheduled fine.
- Profile seed loader runs the same validation gate as the admin
  HTTP handler so a Helm-managed profile and a UI-edited row are
  interchangeable across the audit + admin REST surfaces.

### Doc

New `concepts/runner-profiles` covers: engine scope, default + max
resources, the v0.13.1 fallback, tags, env + secrets, scheduling
hints with the agent-baseline-vs-profile merge contract, services
inheriting only the baseline, chart values for the baseline, and
seeding via Helm.

## v0.13.0 — 2026-06-08

Single feature: **PR-label-driven approval quorum** — the same
gate's quorum changes based on which labels the originating PR
carries. Closes a recurring "hotfix should need one approver, not
two" request without forking pipelines into a parallel hotfix
file.

### Feature — `approval.quorum_by_label`

```yaml
deploy-prod:
  approval:
    approver_groups: [release-approvers]
    required: 2            # baseline (push, manual, tag, …)
    quorum_by_label:
      hotfix: 1            # PR carrying `hotfix` → quorum 1
      breaking-change: 3   # PR carrying `breaking-change` → 3
```

**Semantics**:

- **PR cause only**. Push, manual, tag, upstream, schedule, poll
  all use the baseline `required:` — none of those carry labels.
- **Snapshot at run materialisation**. The PR's labels are read
  once when the run is created (from `runs.cause_detail.pr_labels`,
  itself stamped by the GitHub webhook handler). Relabeling the PR
  after the run is created does NOT recompute the gate; push a
  new head to re-materialise.
- **Multiple labels match → MAX wins**. PR carrying both `hotfix`
  (1) and `breaking-change` (3) lands at quorum 3. Two reasons
  to demand more approvers don't cancel each other.
- **Ties broken lexicographically**. When two labels override to
  the same value, the smallest-named label wins. Load-bearing
  for audit clarity and reproducible tests.
- **Fail-closed defaults**. Malformed `cause_detail` JSON,
  missing `pr_labels` key, or labels-not-array all silently fall
  back to baseline. Failing closed (strict default) is the safe
  direction; failing open would defeat the gate on a parse glitch.

### Validation

Parse-time (surfaces at `apply`, not runtime):

- Label charset: lowercased alphanumeric + `.` `_` `-` `/`.
  `HotFix` in YAML auto-lowers to match what the GitHub webhook
  normaliser stores.
- Override must be ≥ 1; ≤ approvers + approver_groups; cap 16
  entries per gate.
- Empty label keys and case-insensitive duplicate keys rejected.

### UI + audit

- Awaiting-approval card grows a small `label <name>` badge ONLY
  when an override actually fired. Tooltip surfaces "Quorum
  overridden to N by PR label X". No badge on regular gates.
- New audit event `approval.quorum_overridden` carries
  `{base_required, effective_required, label, cause}` metadata.
  Default-quorum gates produce no audit row — the log only
  records the policy events themselves.
- `JobDetail` response now exposes `approval_required` (the
  effective quorum, previously missing from the API entirely)
  and `approval_quorum_label` (omitted when no override fired).

### CI vars

- New `CI_PULL_REQUEST_LABELS` (CSV of the PR's lowercased
  labels) joins the rest of the `CI_PULL_REQUEST_*` family.
  Available in `env:` / `variables:` / plugin `with:`
  substitution; non-PR runs leave it unset so
  `${CI_PULL_REQUEST_LABELS}` reads as literal.

### Provider coverage

GitHub PRs only at v0.13.0. GitLab MR and Bitbucket PR webhooks
don't carry labels into gocdnext yet — those adapters today
process only push events. Follow-up issues track parity:
[#11 GitLab MR](https://github.com/klinux/gocdnext/issues/11),
[#12 Bitbucket PR](https://github.com/klinux/gocdnext/issues/12).

### Latent fix on the way in

Parser's YAML emitter (`/api/pipelines/{id}` "reconstructed YAML"
view) silently dropped `Required` + `ApproverGroups` on the
approval block before this release — the displayed form lost
quorum policy. Fixed in lockstep with the new
`QuorumByLabel` emit so the round-trip is faithful.

## v0.12.0 — 2026-06-07

Two features that close limitations the v0.11 cycle left open:
**isolated workspace mode now supports structured outputs** (Cora's
RWO deployment can finally use the feature), and a new
**`gocdnext/check-pipeline-run@v1` plugin** replaces the inline
`curl + jq` preflight in the trunk-based-release recipe.

### Feature — outputs parity in workspace isolated mode

The v0.11 cycle shipped `outputs:` + `${{ needs.X.outputs.Y }}`
substitution end-to-end for **shared workspace mode only**. Jobs
declaring `outputs:` against an agent in isolated mode
(`accessMode=ReadWriteOnce`) were rejected loud at dispatch with
"switch to ReadWriteMany or fall back to legacy `.gocdnext/*.env`".

That gap is closed. The implementation reuses the housekeeper
sidecar that artefact upload already runs inside:

- **Prep init container** mkdir + touch `.gocdnext/outputs/<jobID>.env`
  inside the pod's ephemeral PVC when `assignment.outputs` is
  non-empty. Permissions are `0o777` on `.gocdnext` and
  `.gocdnext/outputs`, `0o666` on the file, so distroless /
  non-root plugin images can write regardless of pod-level
  umask.
- **Engine** injects `GOCDNEXT_OUTPUT_FILE` on the task container
  via `IsolatedJobSpec.OutputsRelPath`. The path is anchored at
  `workDir` (= scriptWorkDir), not the PVC mount root — a
  checkout with `target_dir:` now writes and reads to the same
  nested path.
- **Agent** reads the file post-task via
  `PodExecutor.Exec("cat -- <abs path>")` inside the housekeeper
  sidecar, parses with the same 64KB cap + charset + dedupe
  pipeline shared mode uses, and ships `JobResult.outputs`
  exactly as before. A capped buffer (`outputsCapBytes+1`)
  defends against a misbehaving plugin OOMing the agent.

Fail-safe contract preserved: task failure short-circuits the
read (no outputs on failed jobs), artifact upload runs first
(operator sees the real root cause when both could fail), and
parse errors fail the job loud with the alias + line number,
never the value.

### Feature — `gocdnext/check-pipeline-run@v1` plugin

The trunk-based-release recipe's `prod.yaml` preflight job
previously embedded `apk add git + git rev-parse + (manual cosign
verify)` as inline shell. The new plugin replaces that with a
typed contract:

```yaml
preflight:
  stage: preflight
  needs: [approve-prod]
  secrets: [GOCDNEXT_API_TOKEN]
  uses: ghcr.io/klinux/gocdnext-plugin-check-pipeline-run@v1
  outputs:
    run_url: RUN_URL
  with:
    api-url: https://gocdnext.example.com
    api-token: ${{ GOCDNEXT_API_TOKEN }}
    project: acme-org
    pipeline: release
    tag: ${TAG}
    expected-status: success
    max-age: 7d
```

Queries the gocdnext REST API and confirms a target pipeline
produced a terminal-success run matching the operator's filter
(tag XOR revision). Fails the gate loud (exit 1) when no match —
the prod deploy chain stays red.

Exit codes split error vs. config so the runbook knows where to
look: `0` match, `1` no match (investigate upstream pipeline),
`2` input validation, `3` API unreachable / auth / shape anomaly
(investigate API/network). Optional `outputs:` ships the matched
run URL so post-deploy notifications audit-link prod promotion
back to the upstream release run that cleared the gate.

Defensive bits: API token in `curl --config` tempfile (never on
argv), token charset rejects whitespace/quote/backslash, tag
charset is Git-refname-derived (not OCI), output file fields
validated against UUID/int/hex/RFC3339 before being written
shell-sourceable, `runs-limit` capped at 100 mirroring the
server's `?runs=N` cap. 25-case smoke harness covers input
validation, API error surfacing, anomalies, and happy paths.

### Latent bug fixed — `target_dir` + outputs in shared K8s

The `workDir`-vs-`mountPath` anchoring fix that isolated mode
needed also applies to shared K8s mode (Docker bind-mount
geometry hid it on Docker). Without this fix, a checkout with
`target_dir: app` would write the outputs file at
`/workspace/app/.gocdnext/outputs/<short>.env` while the env
pointed the plugin at `/workspace/.gocdnext/outputs/<short>.env`,
failing with "No such file or directory" before the parser ran.
Fixed in lockstep so the producer / env / consumer all agree on
the nested path.

## v0.11.0 — 2026-06-07

The single feature of this release is **structured job outputs**
([issue #10](https://github.com/klinux/gocdnext/issues/10)). It
closes the gap that's been forcing downstream jobs to be
`image:` + `script:` with `source .gocdnext/foo.env` whenever
they need a runtime value from a prior job.

### Feature — `outputs:` + `${{ needs.X.outputs.Y }}` substitution

A job declares structured key/value outputs it promises to
produce:

```yaml
jobs:
  bump:
    uses: ghcr.io/klinux/gocdnext-plugin-semver-bump@v1
    outputs:
      next: NEXT       # alias → plugin env-var name
      kind: KIND

  publish:
    needs: [bump]
    uses: ghcr.io/klinux/gocdnext-plugin-buildx@v1
    with:
      image: ghcr.io/org/app
      tags: ${{ needs.bump.outputs.next }}    # resolved at dispatch
```

The agent injects `$GOCDNEXT_OUTPUT_FILE` (engine-aware path —
host for Shell, container `/workspace/<rel>` for Docker/K8s).
Plugins write `KEY=value` lines (same shape as `$GITHUB_OUTPUT`).
The agent parses, filters to the declared subset, rekeys to the
YAML alias, and ships `JobResult.outputs`. The server persists
in a JSONB column on `job_runs` written in the SAME UPDATE as
the success flip, so downstream `needs:`-gated dispatch always
sees the upstream's outputs atomically. The scheduler resolves
`${{ needs.X.outputs.Y }}` against the persisted snapshot at
dispatch time and substitutes into `env:` / `variables:` /
plugin `with:` before sending the JobAssignment.

### Validation + safety

- **Caps**: 64 entries per job (parser); 64KB total payload (sum
  of key+value bytes — enforced agent + server).
- **Alias regex**: `[a-z][a-zA-Z0-9_-]*` (case-sensitive
  end-to-end).
- **Env name regex**: `[A-Za-z_][A-Za-z0-9_]*` (POSIX env-var).
- **Contract**: declared outputs MUST be written by the plugin —
  missing key fails the job loud with the alias + env name in
  the error.
- **Matrix limitation**: a matrix job with >1 row is ambiguous —
  the scheduler errors LOUD listing the matrix keys. Explicit
  per-row selector is roadmap.
- **Kubernetes isolated mode**: declared outputs rejected at
  dispatch (the agent can't yet read `$GOCDNEXT_OUTPUT_FILE`
  from the ephemeral pod filesystem). Use shared
  workspace (`ReadWriteMany`) or fall back to the legacy
  `artifacts:` + `.gocdnext/*.env` pattern.
- **LogMasks**: resolved output values ≥ 8 chars are
  auto-added to the downstream's LogMasks list — defence in
  depth so a digest/token landed in outputs doesn't echo in
  plain text. Documented that outputs are NOT a secret channel;
  use `secrets:` for real credentials.
- **CAS**: outputs are part of the SAME UPDATE as status, so a
  stale `JobResult` rejected by the agent/attempt predicate
  CANNOT write outputs either — the protection is structural.
- **Custom agents**: validation independent of agent code
  rejects malformed outputs server-side (alias regex, UTF-8,
  caps).

### Substitution scope

`${{ needs.X.outputs.Y }}` substitution runs on `env:` /
`variables:` / plugin `with:` — **not** on raw `script:` lines
(so shell-side `${HOME}` etc. survives verbatim). When a
script needs an output value, land it via `variables:` and
reference as `$NAME` inside the script.

### Plugin migrations

- **`gocdnext/semver-bump@v1`** writes `.gocdnext/semver.env`
  (legacy, pre-v0.11 agents) AND `$GOCDNEXT_OUTPUT_FILE` in
  parallel. Operators can declare a subset of `next` / `kind` /
  `current` / `prev_sha`; extras are silently dropped.
- **`gocdnext/image-copy@v1`** same dual-write for
  `promoted_digest` / `source` / `target` / `backend`. New
  example shows the clean shape: cosign-sign stays as
  `uses: gocdnext-plugin-cosign@v1` with
  `image: ghcr.io/org/app@${{ needs.promote.outputs.digest }}`.

### Engine refactor (internal)

The `Engine` interface gained `ScriptSpec.OutputsHostPath` +
`ScriptSpec.OutputsRelPath` so each engine injects
`GOCDNEXT_OUTPUT_FILE` at the path the script will SEE — host
for Shell + Docker fallback, container `/workspace/<rel>` for
Docker containerized + Kubernetes. Fixes the Docker→Shell
fallback case where pre-baked container paths broke
host-execution jobs.

## v0.10.0 — 2026-06-07

The "release of one click" cut. Three features that together close
the trunk-based-release recipe's biggest gap — operator-typed TAG
variables, single-pipeline release+tag conflation, and multi-arch
scan-after-publish trade-offs.

### Feature — pipelines trigger on `event: [tag]` with `CI_TAG_*` env vars

Tag pushes now route to pipelines that declare `when.event: [tag]`
(or git materials with `on: [tag]`). The routing is URL-only —
tags don't carry a base branch (a tag points at a SHA that may
not be on any branch), so the URL+branch fingerprint used by the
branch-push path can't fire. The new path matches by URL,
filters by per-material Events list, and stamps `cause="tag"` +
`cause_detail={tag_name, tag_message, tag_sha, tagger}` on the
run.

The scheduler emits three env vars from `cause_detail` for any
run where `cause == "tag"`:

| Var | Source | Notes |
|---|---|---|
| `CI_TAG_NAME` | `pr_head_ref` equivalent — the tag name | Always present on tag runs |
| `CI_TAG_MESSAGE` | head commit message | Lightweight tags only; annotated tags omit |
| `CI_TAG_AUTHOR` | head commit author | Same nil-tolerance |

The git ref target SHA arrives via the existing `CI_COMMIT_SHA` —
NOT a separate `CI_TAG_SHA`, deliberately, so operators don't
misread it as an OCI image digest (which it isn't). For image
refs in cosign-sign and similar, use `${CI_TAG_NAME}` (cosign
resolves to the manifest digest at sign-time).

Parser now validates `when.event:` and git material `on:` against
the accepted enum — typos like `event: [tags]` (note the plural)
or `on: [tagg]` fail at apply-time with a clear error instead of
silently producing a pipeline that never fires.

### Feature — `gocdnext/semver-bump@v1` plugin

Auto-computes the next SemVer from Conventional Commits since the
prior tag. Writes a shell-sourceable `.gocdnext/semver.env` that
downstream `create-tag` jobs `source`. Combined with `event: [tag]`,
the release flow becomes "click Run on release.yaml →
semver-bump → create-tag → push; tag webhook auto-fires tag.yaml"
with no operator-typed TAG anywhere.

Bump rules: `feat!:` / `fix!:` (etc.) or `BREAKING CHANGE:` in
body → major; `feat:` → minor; else → patch. Special kinds:
`initial` (no prior tag, emits PLUGIN_INITIAL) and `none`
(NEXT=CURRENT, downstream branches on KIND).

Security hardening across two review rounds: prefix charset
`[A-Za-z0-9._/-]*` (the value lands in the sourced output file,
so shell injection via prefix was the original HIGH/SEC finding);
output path rejects absolute and `..` traversal; pre-release
validated `[A-Za-z0-9.-]+`; SIGPIPE in conventional-commits scan
replaced with here-strings (`echo | grep -q` under `pipefail`
silently misclassified a `feat:` with a large body as patch);
`git describe --match '<prefix>[0-9]*'` filters non-SemVer tags
sharing the prefix (`vfoo`, `vnext`, `vendor-*`).

### Feature — `gocdnext/image-copy@v1` plugin

Promotes multi-arch images between registries preserving the
manifest list — what `gocdnext/docker-push` can't do because
`docker tag` + `docker push` loses the index. Three
interchangeable backends:

- `crane` (default): single static binary, fast, multi-arch
  native
- `skopeo`: broader OCI tooling with `--multi-arch all` explicit
- `buildx-imagetools`: when the job already declares `docker: true`

Always emits `PROMOTED_DIGEST` to a workspace file so a
downstream cosign-sign step can anchor by digest rather than the
mutable tag, closing the "what got signed?" race. Missing digest
fails the job loud (exit 3) — the digest is the central output
of this plugin and silently emitting empty would push the failure
downstream with a confusing error.

Security hardening across four review rounds:

- Authfile lives in mktemp 0600 dir; EXIT/INT/TERM trap wipes
  on every exit path
- Cross-registry creds: target token defaults to source ONLY on
  same-host promotion; cross-host without explicit source creds
  leaves the source anonymous (no silent token leak to a
  stranger)
- buildx branch exports `DOCKER_CONFIG=<tempdir>` before
  `docker login`, so credentials land in the trap-cleaned tempdir,
  not `$HOME/.docker/config.json`
- Source/target refs charset-validated; Docker Hub shorthand
  (`org/app:tag`) rejected — image-copy is cross-registry, refuses
  ambiguous refs
- Target enforced tag-form only (no `@digest`); primary tag and
  every `extra-tags` entry matched against the OCI tag spec
  `[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}`
- `docker-cli` + `docker-cli-buildx` installed in the image so
  the buildx-imagetools backend has its binaries (`docker: true`
  on the job only mounts the daemon socket, not the CLI)

None of the three backends transfer cosign signatures /
attestations today (those live as separate registry artifacts via
the cosign triangulation). Re-sign at the target on the emitted
`PROMOTED_DIGEST` instead — same security property, immutable
chain. Native cosign-signature preservation is roadmap (future
`cosign-copy` backend wrapping `cosign copy SRC DST`).

### Docs

- New "Tag-push runs" section in YAML reference, listing
  `CI_TAG_*` vars + the OCI-digest caveat
- `trunk-based-release` recipe: model-mental update,
  reworked "Why one pipeline vs. split release + tag.yaml"
  section explaining the choice, new "Variant: split release +
  tag.yaml" with the cleaner shape now possible
- Limitations section marks `Tag-push event`,
  `Semver bump as plugin`, and `Multi-arch scan-before-publish`
  all as ✅ shipped

### Plugin catalog

44 plugins total (was 42 in v0.9.0).

## v0.9.0 — 2026-06-07

### Feature — `CI_CAUSE` + `CI_PULL_REQUEST_*` env vars (closes #9)

The webhook handler has stamped PR metadata on `runs.cause_detail`
since migration 00001 (pr_number, pr_title, pr_head_ref, pr_base_ref,
pr_author, pr_url), but the data never reached the agent's job env.
Pipelines wanting sonar PR decoration / ai-review PR comments had to
thread the data through external trigger plumbing.

This release runs the data the last mile. PR-triggered runs now see:

| Var | Source |
|---|---|
| `CI_CAUSE` | `pull_request` (or `webhook`, `manual`, `upstream`, `schedule`, `poll`) |
| `CI_PULL_REQUEST_KEY` | `pr_number` |
| `CI_PULL_REQUEST_BRANCH` | `pr_head_ref` |
| `CI_PULL_REQUEST_BASE` | `pr_base_ref` |
| `CI_PULL_REQUEST_TITLE` | `pr_title` |
| `CI_PULL_REQUEST_AUTHOR` | `pr_author` |
| `CI_PULL_REQUEST_URL` | `pr_url` |

`CI_CAUSE` ships on every run (when non-empty), enabling
`if: "$CI_CAUSE == manual"` branching. Non-PR runs (push, manual,
upstream, schedule, poll) skip `CI_PULL_REQUEST_*` silently.

**Backward compat absolute**: no migration, no proto, no rename.
Missing fields stay UNSET rather than empty so the substitution
layer leaves `${CI_PULL_REQUEST_TITLE}` literal on the rare PR
with no title — no `myapp:pr-` style tags. Legacy runs with empty
cause / nil cause_detail / malformed JSON all degrade silently.

**Catalog source-of-truth updated**: `plugins/sonar`, `ai-review`,
`buildx`, `docker` examples switched to `${CI_PULL_REQUEST_KEY}` +
`${CI_PULL_REQUEST_BRANCH}` + `${CI_PULL_REQUEST_BASE}`. The
trunk-based-release concept doc drops its `variables:` workaround
block; pipelines are now single-pass.

### Fix — dashboard sidebar collapse persists across reloads

shadcn's `<Sidebar>` had been writing the `sidebar_state` cookie on
every toggle, but the dashboard layout never read it back SSR-side.
Collapsed sidebars flashed open on every reload before client
hydration corrected them (and after a hard refresh, didn't always
correct). [`(dashboard)/layout.tsx`](web/app/(dashboard)/layout.tsx)
now reads the cookie and threads it into `<SidebarProvider
defaultOpen={...}>` so the rendered markup matches the user's last
choice immediately.

### Fix — docs content centering on wide screens

Starlight's default layout (`TwoColumnContent.astro:48`) pushed the
content panel rightward via `--sl-content-margin-inline: auto 0`
when both sidebar and TOC were visible. On 1920+px monitors the
result was a heavy left gap (~285px) and a tight right gap (~110px)
— visually lopsided. `brand.css` now re-centers the panel and
widens `--sl-content-width` from 45rem to 52rem so prose breathes
without crowding the TOC.

### Docs

- README aligned with v0.8.0 reality (status, differentiators,
  quick-start, Helm version, shipped/open replacing the phase-0
  roadmap).
- YAML reference gains `CI_CAUSE` row + dedicated pull-request
  section listing the six new env vars.

## v0.8.0 — 2026-06-06

### Feature — gocdnext/ai-review plugin (Claude + OpenAI)

New plugin runs an LLM-driven code review against the PR diff and
(optionally) posts the verdict as a PR comment. Supports two
providers out of the box: Anthropic (Claude) via
`provider: claude` and OpenAI (gpt-4 family) via `provider:
openai`. The cost guardrail is `max-diff-bytes:` (default 50000) —
the diff is truncated to that ceiling before being sent to the
LLM, so a PR with a 5MB lockfile churn doesn't blow up token
spend.

Security: no API keys land on argv. The plugin writes a curl
`--config` file (mktemp 0600 + EXIT/INT/TERM trap cleanup) for
the Authorization header, and the SCM PR-comment token follows
the same pattern. `parse_bool` / `parse_int` / `parse_float_0_to_1`
strictly validate user inputs; subshell exit codes propagate via
`|| exit $?`. Output paths are `LC_ALL=C` bash-substring trimmed
(no SIGPIPE on giant diffs).

### Feature — cosign plugin: `key-content:` input

The signing key can now be piped inline through `secrets:` +
`with: { key-content: ${{ COSIGN_PRIVATE_KEY }} }`. The plugin
writes the PEM bytes to a 0600 tempfile internally and a trap
wipes it on every exit path (no `exec cosign` — child process so
the trap actually fires). The legacy `key:` input remains FILE
PATH only — a runtime guard rejects PEM-like content and inline
multi-line values with a remediation hint pointing at
`key-content:`. The displayed cosign command redacts the value
after `--key`, `--key-password`, `--password`, `-p` regardless.
This removes the only pattern that persisted the private key in
the artifact backend.

### Feature — trivy plugin: registry credentials

`username:` / `password:` inputs promoted to `TRIVY_USERNAME` /
`TRIVY_PASSWORD` env which trivy reads natively. Scan-after-
publish pipelines on a private registry now have a clean path —
the build job's `docker login` doesn't carry across job
containers, so trivy needs its own creds. Values flow through
the agent's env-propagation path (NAME-only on argv, value via
cmd.Env), so they don't appear in `ps auxww`.

### Security — agent docker engine: env values off argv

The docker engine previously emitted `-e KEY=VAL` on docker run's
argv. Secrets injected via the plugin contract (registry tokens,
cosign passwords, API keys) were visible to anyone with `ps`
access on the host. The engine now emits `-e KEY` (name-only) on
argv and propagates the value through `cmd.Env` of the docker
CLI invocation — docker reads the value from its own environment
and forwards it into the container. Same fix applied to service
containers via `kubernetes_services.go`. Regression tests in
`docker_envleak_test.go` assert no secret value ever appears in
the argv slice, including multi-line PEMs.

### Concept doc — trunk-based release

New `/concepts/trunk-based-release/` walks a 4-pipeline trunk
model (pr.yaml → main.yaml → release.yaml → prod.yaml) with
production-grade YAML for each stage. Covers approval gates,
cosign signing via `key-content`, multi-arch + scan-after-publish
trade-off, GIT_ASKPASS pattern for tagging without leaking the
release token, and a manual-verification preflight on the prod
deploy.

### Docs — docker-build + helm-release recipes overhauled

The Docker recipe was rewritten to match how production multi-
arch + signed-image pipelines actually compose: build (push:true,
multi-arch) → trivy-scan (registry API, no docker socket) →
cosign-sign (registry API + key-content). The conceptually broken
"single-arch scan-before-publish" variant was removed. The Helm
recipe's sign-chart block now uses `key-content:` + registry
creds + drops `docker: true` to match.

## v0.7.0 — 2026-06-06

### Feature — gocdnext/sonar plugin (single image, SQ + SonarCloud)

New plugin covers Sonar's three scanner front-ends in one image:
`mvn sonar:sonar`, `gradle sonar`, and the language-agnostic
Scanner CLI for JS/TS, Python, Go, etc. Mode auto-detected from
project layout (pom.xml / build.gradle{,.kts} / neither) or
overridden via `mode:`. SonarCloud is the default host; point
`host-url:` at the install for self-hosted.

Security: token never lands on argv (`SONAR_TOKEN` env). The
`extra-props:` input is parsed line-by-line so values with
whitespace stay one argv, and auth-bearing properties
(`sonar.token`, `sonar.login`, `sonar.password`) are rejected at
runtime case-insensitively. Supply-chain: SHA256 of
sonar-scanner-cli 6.2.1.4610 and gradle 8.10 binaries pinned in
the Dockerfile (verified on every build, fetched from the
official .sha256 files at SonarSource + gradle.org).

Performance: `SONAR_USER_HOME`, `MAVEN_LOCAL_REPO`,
`GRADLE_USER_HOME` default to absolute `/workspace/*` paths so
`cache: paths: [.m2-repo]` etc. align regardless of working-dir
(no silent monorepo cache miss). Quality-Gate wait opt-in
(`wait-for-quality-gate: "true"`) blocks the PR pipeline until
the gate verdict — default off because the wait adds 1-3 min.

### Feature — go/maven/gradle plugins: cache + testcontainers + safer bool inputs

JVM- and Go-toolchain plugins gained perf knobs aimed at the
common "why is CI so slow" answers + tighter input validation.

go:
- `cgo:` toggle (`true`/`false`) exposes `CGO_ENABLED` without
  having to set `variables:`.
- Cache example now `{{ hash "go.sum" }}`-keyed instead of the
  broken `${CI_COMMIT_BRANCH}` literal (shell-style vars don't
  expand in cache keys — that's documented now too).

maven:
- New `maven-opts:`, `parallel:` (`-T <val>`), `build-cache:`
  inputs. `--batch-mode --no-transfer-progress` always-on (kills
  the "Downloading…" wall on cold runs).
- Cache example re-keyed on `{{ hash "**/pom.xml" }}` so a
  dependency bump in ANY module of a reactor invalidates.
- Build Cache Extension toggle (`-Dmaven.build.cache.enabled`)
  for projects on Apache Maven 3.9+ with the extension
  registered.

gradle:
- New `build-cache:`, `parallel:`, `configuration-cache:`,
  `args:` inputs. All three cache toggles are TRI-STATE: unset
  passes NO flag (respects the project's `org.gradle.*` in
  gradle.properties); `"true"` forces `--build-cache` etc;
  `"false"` forces the `--no-*` flag. Avoids silently overriding
  projects that opted in via gradle.properties.
- Cache example keyed on `**/*.gradle*` + wrapper props; a
  second variant adds `gradle/libs.versions.toml` for version-
  catalog projects.

Testcontainers (all three): plugin auto-exports
`TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE` +
`TESTCONTAINERS_HOST_OVERRIDE` ONLY when `/var/run/docker.sock`
is actually mounted into the task container. Doesn't trigger on
`DOCKER_HOST`: the Kubernetes engine path uses DinD with
`DOCKER_HOST=tcp://localhost:2375` and no socket — explicit
overrides would point at a non-existent path; Testcontainers'
own resolver handles DinD via DOCKER_HOST. Docker engine path
keeps working.

Bool input validation: new POSIX-safe `parse_bool` helper
accepts `true|false|1|0|yes|no|on|off` case-insensitive, `exit 2`
with a clear error on anything else. Wired through every bool
input across go/maven/gradle/sonar. Call sites use
capture-then-test (`val=$(parse_bool ...) || exit $?`) so a
subshell `exit 2` from parse_bool propagates to the parent
script instead of being swallowed by `$(...)`. Smoke: typos
like `cgo: flase` now abort with rc=2 and never run the
toolchain.

### Fix — plugin `uses:` references pointed at non-existent Docker Hub paths

Plugin catalog + every example + recipe documented
`uses: gocdnext/<name>@v1`, which `ResolvePluginRef` translates
to a Docker image `gocdnext/<name>:v1` at docker.io — an image
that doesn't exist. Published images live under
`ghcr.io/klinux/gocdnext-plugin-<name>:vN`. Replaced every
reference (docs + recipes + plugin.yaml examples +
.gocdnext/ test pipelines) with the canonical pullable form
`ghcr.io/klinux/gocdnext-plugin-<name>@vN`. 58 files touched
mechanically.

### Fix — plugin catalog page anchors

The generator at `docs/scripts/gen-plugin-catalog.mjs` rendered
each plugin heading as `## name {#name}` (Pandoc-style explicit
anchor). Starlight doesn't honor that syntax — its slugifier
turns the entire heading text `name {#name}` into
`id="name-name"`, so every "At a glance" link 404'd in-page.
Switched to plain `## name` and let Starlight's auto-slugifier
produce `id="name"` from heading text. Verified the rendered
HTML: `id="ansible"`, `id="buildx"`, `id="trivy"`, etc. all
resolve.

### Docs — comprehensive rewrite to match shipped behavior

Two adversarial audit passes turned up wide drift between the
docs site and the actual code/UI/parser surface. Catch-up pass:

YAML reference: `when.branch` is SINGULAR (the parser rejects
`branches:`/`paths:`/`tag_name:`); `approval:` uses
`approver_groups` + `required` (not `groups`/`quorum`);
`artifacts.optional` + `test_reports` are bare `[]string` (no
`paths: {}` wrapper); `parallel.matrix` is list-of-objects;
notifications `on:` accepts `canceled` (single l); substitution
grammar is identifier-only — dotted `${{ secrets.X }}` is
rejected; services are pipeline-level only.

All 12 recipes rewritten: `branches → branch`,
`${{ secrets.X }} → ${{ X }}`, plugin `with:` keys re-aligned
against `plugins/*/plugin.yaml`, 12/12 now parser-clean.

Install/reference docs: helm/upgrade version pinned to 0.6.4
across the board, v0.5.0 BREAKING DEFAULT callout for workspace
accessMode flip, env-vars gained the
`GOCDNEXT_K8S_WORKSPACE_*` set, auth.md callback URLs corrected
(`/auth/callback/X` not `/api/v1/auth/oauth/X/callback`),
webhooks.md paths corrected (`/api/webhooks/X` not
`/api/v1/webhook/X`), api-tokens.md page is `/account` (not
`/settings/api-tokens`), cli.md rewritten to the 5 real
subcommands (removed 8 fictional ones), api.md endpoint
corrections (`/job_runs/{id}/rerun|approve|reject` not
`/runs/{id}/jobs/{jobID}/...`).

Concept docs: materials.md correctly documents `cause:
schedule` (not `cron`); cache.md rewritten with accurate
template grammar (only `{{ hash "glob" }}` expands — `${VAR}`
stays literal in cache keys); architecture.md +
runner_profile_env_secrets aren't a separate table, they're
JSONB columns on `runner_profiles` (migration 00030).

New concept pages: `concepts/kubernetes-runtime.md` (shared vs
isolated, init+task+housekeeper pod model, RBAC) and
`concepts/services.md` (sidecar lifecycle, sticky-failed
semantics, Setup column + project-card representation).

Plugin reference fixes: node v2 full rewrite (install/manager/
frozen/prod inputs, shell-eval command, yarn v1 rejection);
gitleaks gained allowlist-paths + verbose + redact; trivy
gained skip-db-update + cached DB example; golangci-lint +
terraform gained cached examples.

### Chore — scrub internal customer references from public repo

Replaced `cora`, `corabank`, `corapulse`, `img.cora.tools` with
generic placeholders (`registry.example.com`, `@app/web`,
`acme-org`, `monorepo-app`, `gocdnext.example.com`) across
tests, source comments, plugin examples, and CHANGELOG prose.
No behavior change; the affected tests stayed green
(`TestSubstituteRefs`,
`TestBuildAssignment_SubstitutesPluginSettings`,
`TestGitHubWebhook_PushFansOutToEveryPipeline`).

### Migration notes

- Operators using `uses: gocdnext/<name>@v1` in apply'd
  pipelines need to switch to
  `uses: ghcr.io/klinux/gocdnext-plugin-<name>@v1`. The catalog
  short-name lookup still validates inputs, but the runtime
  image pull always tried docker.io/gocdnext/X and failed —
  meaning these pipelines were already broken at runtime; the
  fix just makes the form match what actually works.
- Gradle plugin's `build-cache:` / `parallel:` /
  `configuration-cache:` inputs are now TRI-STATE. Previous
  behavior (when these inputs existed in the v0.7.0 dev cycle
  only) was bi-state with default false; legacy `@v1`
  consumers were unaffected because the inputs were new. No
  external behavior change for projects that didn't set them.
- Maven plugin's `--no-transfer-progress` is now always-on —
  if you were grepping the log for "Downloading…" lines you
  won't find them anymore. Replace with surefire-reports
  parsing or the test_reports/Tests tab.

## v0.6.4 — 2026-06-06

### Feature — services compact view on the project page pipeline cards

The project page's `PipelineCard` now renders a compact "services"
box as the first item in its stage strip (left of stage 1) when
the latest run declared services. One circle per service, status
colour from the shared TONE palette (`ready/stopped → success`,
`starting → running`, `failed → failed`), tooltip with name +
image + status + error when failed. Mirrors the Setup column on
the run-detail page so the operator's eye reads the same
vocabulary on both surfaces — except the card-sized version
trades the popover for a tooltip to fit the density.

### Backend — `RunSummary.has_services` exposed end-to-end

`runs.has_services` (migration 00036's snapshot of
`pipeline.Services` non-emptiness at run-create time) was
already in the DB but didn't reach the API. v0.6.4 selects it in
the four queries that source `RunSummary`:

- `LatestRunPerPipelineByProjectSlug` (project page latest run +
  VSM nodes)
- `ListRunsByProjectSlug` (project page recent runs list)
- `GetRunWithPipeline` (run detail page)
- `ListRunsGlobal` (dashboard + `/runs` page global timeline)

…and stamps it into every `RunSummary{}` constructor in
`store/reads.go`, `store/dashboard.go`, and `store/vsm.go`. The
OpenAPI spec marks `has_services` as required on `RunSummary`
with a comment explaining the snapshot semantics.

### Perf — services fetch on project page gated, not blanket

Without v0.6.4 the project page issued one
`GET /api/v1/runs/:id/services` per pipeline card every 5 s
while any run was live, even for pipelines that never declared
services. With `has_services` in the read model, `PipelineCard`
runs `useQuery({ enabled: !!run && run.has_services })`, so
pipelines without services contribute zero requests and zero
polling intervals.

### A11y — `ServiceCircle` on the card is a real button

The compact service node on the project card now renders as
`<button type="button">` with `aria-label="Service <name>:
<status>"`, a `title` fallback, and `focus-visible:ring`. Screen
readers announce role + label; keyboard users can Tab onto it;
the inner status glyphs (Loader2/X/Check) get `aria-hidden` so
the row doesn't read twice.

## v0.6.3 — 2026-06-05

### Fix — `stopped` services no longer paint Setup as broken; consolidate services polling

Two follow-ups on v0.6.2's pipeline-canvas integration:

**`stopped` is the happy-path cleanup, not a dim/skipped state.**
v0.6.2 mapped `stopped` to the `skipped` tone in both
`aggregateServicesStatus` and `servicePillStatus`. The
run-terminal cleanup broadcast fires `stopped` on EVERY
successful run, so as soon as the run finished the Setup
column + connector flipped to the dimmed "skipped" look —
visually claiming a prereq was broken on every clean run.
The Services tab already used `neutral` for stopped, so the
two views disagreed.

Fix:
- `aggregateServicesStatus` reduced to `failed > starting >
  success`. `stopped` folds into success.
- `servicePillStatus` maps `stopped → success` (cleanup-on-
  terminal is the happy path). v0.6.1's sticky-failed in the
  store keeps a true failure visible even after the cleanup
  pass, so this fold can't hide a real broken service.

**`["run-services", runId]` polling consolidated to one
observer.** v0.6.2 had three concurrent `refetchInterval`s:
the canvas (always mounted), the tab-strip badge counter, and
the Services tab content. React Query's shared cache dedupes
the fetch but each observer still kept its own interval
timer. `PipelineCanvas` is now the single polling source; the
badge query and the Services tab subscribe to the cache
without `refetchInterval`. Reduces the number of running
intervals from 3 → 1 per visited run-detail page.

## v0.6.2 — 2026-06-05

### UX — services as inline nodes in the pipeline graph (Woodpecker-style)

Issue #7's first cut buried services in a dedicated Services
tab on the run-detail page. The operator's mental model is
"services are prerequisites that have to be up BEFORE the
pipeline can run" — and Woodpecker's UI already trained that
intuition by rendering services as graph nodes alongside
stages. v0.6.2 aligns gocdnext with that shape.

- `PipelineCanvas` now renders a virtual "Setup" column as the
  FIRST column when the run declares services. Each service is
  a node (same pill style as job nodes) with the status glyph,
  name, and live duration.
- Status mapping shares the existing TONE palette so the same
  colour vocabulary covers services + jobs + stages:
  `ready → success`, `starting → running`, `stopped → skipped`,
  `failed → failed`.
- Click on a service node opens a popover with image, pod name,
  per-state timestamps (`started`/`ready`/`stopped`), and the
  full error message when status is `failed`.
- A connector chevron joins the Setup column to stage 1; its
  colour follows the worst service status so a failed prereq is
  visible at a glance, not just inside the popover.
- The "Services" tab stays as the detail/tabular view — both
  reads share the same react-query cache via the
  `["run-services", runId]` key.

The Services tab list, the new graph nodes, and the popover all
poll on the same 3-second cadence while the run is live.

### Project header — drop duplicate breadcrumb

`/projects/<slug>` had a `Projects > <slug>` breadcrumb sitting
right above the project's own name + description. The breadcrumb
echoed information already visible 12 pixels below it and added
no navigation value (the breadcrumb's only target was `/`, which
the global side nav already covers). Removed.

## v0.6.1 — 2026-06-05

### Fixes — v0.6.0 ServiceLifecycle integrity follow-up

Three semantic gaps caught in the v0.6.0 post-review:

1. **`stopped` could overwrite `failed`.** v0.6.0's
   `UpsertServiceRun` unconditionally wrote `EXCLUDED.status`, so
   the cleanup broadcast's `stopped` event (which fires on EVERY
   run — successful or not) erased the failure status of a
   service that had blown up. The UI then showed "stopped" and
   the operator chased the wrong root cause.

   Fix: SQL guard `status = CASE WHEN service_runs.status =
   'failed' THEN service_runs.status ELSE EXCLUDED.status END`.
   Once failed, the row stays failed. `started_at`/`ready_at`/
   `stopped_at` keep their COALESCE behaviour so timestamps
   still accrue, but the visible status stays honest.

2. **No ownership check on `ServiceLifecycle`.** Any
   authenticated agent could write any `run_id`'s service
   lifecycle. Worst-case: a bug/compromise on one agent
   poisoning the Services tab of another tenant's run.

   Fix: new `AgentOwnedJobInRun` query +
   handler-side gate. For `starting`/`ready`/`failed` events,
   the agent must own at least one `job_run` of the run.
   `stopped` falls back to a cheap `RunExists` check because
   cleanup is broadcast to k8s-capable agents that may
   legitimately never have owned a job — but at least the
   `run_id` must be a real row, defanging random-UUID spray.

3. **Reuse-from-sibling pods didn't emit `ready`/`failed`.**
   The v0.6.0 engine gated both events on
   `if created[svc.Name]`, but the contract docstring on
   `Engine.EnsureServices` already promised the reuser would
   emit. When the original creator's stream died mid-wait,
   the row stayed with `status=starting` forever.

   Fix: drop the `created` gate around `ready`/`failed`.
   Concurrent writes from the creator + reuser are safe
   because the server's COALESCE-preserving upsert keeps the
   first-observed timestamps. `starting` stays creator-only
   because the reuser literally didn't issue Create.

Test additions:
- `TestUpsertServiceRun_FailedIsSticky_StoppedDoesNotOverwrite`
- `TestAgentOwnedJobInRun_TrueWhenAgentRanAJob`
- `TestAgentOwnedJobInRun_FalseForMissingRun`

## v0.6.0 — 2026-06-05

### Feature — pipeline services tracked in the UI (closes issue #7)

Run-scoped service Pods (shipped in v0.4.35) used to be a server
blind spot: the agent created/destroyed them, the server only
heard about it at run-terminal via `CleanupRunServicesResult`,
the UI never saw the rows at all. A service crashing at start
manifested as "every test job times out with connection refused"
and the operator chased the wrong root cause.

v0.6.0 wires the full chain:

- New `service_runs` table (migration 00039), keyed on
  `(run_id, name)`. Tracks `starting`, `ready`, `stopped`, `failed`
  with per-state timestamps so the UI can render the readiness
  window AND the total uptime.
- New `ServiceLifecycle` proto on `AgentMessage` (field 7), emitted
  by the Kubernetes engine at three transition points:
  - `starting` after `Pod Create` succeeds (skipped for sibling
    reuse so `started_at` anchors to the FIRST agent).
  - `ready` when `waitForPodIP` succeeds.
  - `failed` if `waitForPodIP` errors (image pull backoff,
    startup timeout).
  - `stopped` emitted by `CleanupRunServices` per successful
    delete (NotFound from a sibling-race doesn't emit, otherwise
    `stopped_at` would clobber across agents).
- New server handler `handleServiceLifecycle` in `grpcsrv/connect.go`
  that validates + clamps agent-supplied strings (image, pod_name,
  error) and `UpsertServiceRun` into the store. Status enum is
  validated against the closed `starting|ready|stopped|failed`
  set; unknown values drop with a warn.
- New API endpoint `GET /api/v1/runs/{id}/services` returning the
  alphabetically-ordered list as `ServiceResponse[]`.
- New "Services" tab on the run-detail page in `web/`, polling
  every 3s while the run is live. Each row shows name + image +
  status pill (`ready`=success, `starting`=running, `stopped`=neutral,
  `failed`=destructive), `started`/`ready` relative-times, and a
  duration that flips between "ready window" (live) and "total
  uptime" (stopped).

The `Engine.EnsureServices` and `Engine.CleanupRunServices`
interfaces gained an `onLifecycle func(ServiceLifecycleEvent)`
trailing parameter. Shell + Docker engines accept it as a no-op
(neither hosts services today). All existing tests / stubs
updated to the new signature.

Store tests cover the COALESCE-preservation contract:
re-issued `ready` doesn't reset `ready_at`, an out-of-order
`starting` after `ready` doesn't clobber `ready_at` either.

**Bonus — service Pod logs** are still a follow-up; the issue's
"why did postgres die?" log viewer needs its own log-line
partition shape and is tracked separately.

## v0.5.7 — 2026-06-05

### Fix — cache store refreshes the row to empty when nothing to cache; defangs leading-`-` paths

Two follow-ups to v0.5.6's `StoreFromPod` rewrite:

1. **All-paths-missing was poisoning the cache row.** v0.5.6's
   shell wrapper exited 0 with empty stdout when no declared
   path existed (cold start, conditional output). The agent
   then PUT a 0-byte blob and called `MarkCacheReady` with the
   sha of nothing. Downstream `Fetch` saw `Found=true`,
   downloaded 0 bytes, and `DownloadAndUntar`'s
   `gzip.NewReader` errored — every subsequent run with the
   same key failed cache restore until manual eviction.

2. **`tar -T` reopened the leading-`-` foothold.** v0.5.5's
   raw tar used `-- <path>` to defuse paths starting with `-`.
   The v0.5.6 rewrite read paths from `tar -T <file>`, where
   some tar implementations (and `[ -e "$p" ]` itself) may
   misread a `-prefixed` entry as a flag.

Fix: rewrite `StoreFromPod` to do TWO execs:

- **Probe** (`sh -c <probe-script>`): list existing paths
  with a `case "$p" in -*) p="./$p" ;;` rewrite so the
  defanged form (`./-dist`) reaches both `[ -e ]` and
  downstream tar. One round-trip per cache entry, output
  parsed agent-side.
- **Tar** (only if probe returned ≥1 survivor):
  `sh -c <tar-script>` over the filtered list, then PUT +
  `MarkCacheReady` as before.

When the probe returns NOTHING, `StoreFromPod` doesn't skip
the RPC — it uploads a valid-empty `tar.gz` (built agent-side
via `runner.TarGzPaths("", nil)` so the empty/non-empty paths
share the exact encoding). The cache row gets a fresh empty
ready blob, mirroring shared-mode behaviour: a previous run's
populated ready blob is REPLACED rather than preserved. This
is the round-7 follow-up to the earlier "skip RPC" approach,
which would have left a stale (and possibly large) row alive
on the cache backend whenever a job stopped producing the
cached path.

Test additions in `agent/internal/rpc/cache_test.go`:
- `TestStoreFromPod_HappyPath_TwoExecsAndFullRPC` — argv
  shape for probe + tar, full RPC sequence, missing path
  filtered between execs.
- `TestStoreFromPod_AllPathsMissing_UploadsValidEmptyTarGz` —
  asserts ONE exec (probe only), `RequestCachePut` + PUT +
  `MarkCacheReady` all fire with a non-empty Content-Length,
  and the ready row's `size_bytes` matches the PUT body.
- `TestStoreFromPod_DefangsLeadingDashPath` — tar argv must
  carry `./-dist`, never raw `-dist`.

`recordingExecutor` extended to drive per-call stdout/err
payloads so the two-exec dance is testable without a real
cluster.

## v0.5.6 — 2026-06-05

### Fix — `StoreFromPod` skips missing paths instead of failing the cache

v0.5.5 issued `tar -czf - -C <workdir> -- <path1> <path2> …`
inside the housekeeper. If ANY declared path was missing (cold
start, conditionally-generated output, partial build), tar
exited non-zero and the whole cache failed to upload. Cache
fetches kept working but the store side never populated, so
the next run still got a miss — the worst-case feedback loop
(operator thinks cache is working because restore is silent
on miss, but it's never being written).

Fix: wrap tar in an in-pod shell script that filters out
missing paths first, writes the surviving list to a tempfile,
then `exec tar -czf - -T <tmpfile>`. Mirrors the shared-mode
`TarGzPaths` semantics (skip ENOENT silently). Paths with
spaces survive because `tar -T file` reads one entry per line.

If ALL paths are missing (e.g. cold start with no build
output to cache), the script exits 0 with empty output; the
downstream PUT uploads the gzip-empty-tar envelope (~30 bytes)
and `MarkCacheReady` flips the row. Effectively a no-op
store. Mirrors shared-mode behaviour where TarGzPaths returns
an empty archive.

Test additions in `agent/internal/rpc/cache_test.go`:
- `TestStoreFromPod_FiltersMissingPaths` — asserts the cmd
  argv shape uses `sh -c <script>` with `[ -e "$p" ]` filter
  and `tar -czf - -T <file>`, plus the full RPC sequence
  (RequestCachePut → exec → PUT → MarkCacheReady).
- `TestStoreFromPod_EmptyPathsRejected` /
  `TestStoreFromPod_NilExecutorRejected` — input guards.
- `TestResolveGet_FoundReturnsTicket` /
  `TestResolveGet_NotFoundIsNoError` /
  `TestResolveGet_NotFoundCodeIsNoError` — ticket round-trip
  + NotFound normalisation.

Stale comments in `postjob.go` and `prep.go` that still
described caches as "no-op in isolated mode" updated to
reflect the v0.5.5 behaviour.

## v0.5.5 — 2026-06-05

### Feature — literal-key cache fetch + store in isolated mode

v0.5.0–v0.5.4 made `cache:` a no-op in isolated mode with a
warning, on the assumption that an in-pod gRPC session was the
only way to call `RequestCacheGet`/`Put`. Wrong: the agent
already holds the session and can pre-resolve at dispatch,
identical to the way it pre-signs `artifact_downloads`.

How it works now:

1. `CacheEntry` proto gains three additive fields: `fetch_url`,
   `fetch_sha256`, `fetch_found`. Empty on the wire from
   server → agent; the agent stamps them at dispatch.
2. Before pod creation, `executeIsolated` walks
   `a.GetCaches()`, calls
   `IsolatedCache.ResolveGet(runID, jobID, key)` for each
   literal key, and writes the ticket back into the proto.
   Templated keys (`{{ hash "..." }}`) are left empty.
3. `proto.Marshal(a)` serialises the populated assignment;
   the Secret carries it.
4. Init container's `Prep` iterates caches: literal hit →
   HTTP GET on `fetch_url` + untar over `scriptWorkDir`;
   literal miss → silent; templated → explicit warning.
5. After task success, `PostJob` calls
   `IsolatedCache.StoreFromPod` per literal key: exec
   `tar -czf - -C scriptWorkDir -- <paths…>` inside the
   housekeeper, stream through a temp file (S3/GCS need
   Content-Length), PUT to the signed URL from
   `RequestCachePut`, then `MarkCacheReady`.

Templated keys remain skipped in v0.5.5 — the in-pod hashing
needs a workspace, and we don't yet have a way to ship the
signed URL back into the init container. Trivy + similar
literal-keyed caches (the user-visible motivation: 95 MiB
`trivy-db` re-downloaded every run) now restore on the first
hit.

Test additions in
`agent/internal/runner/prep_test.go`:
- `TestPrep_CacheHitDownloads` — happy-path fetch via httptest
- `TestPrep_LogsTemplatedKeyLimitation` — warning preserved
  for `{{ }}` keys
- `TestPrep_CacheMissIsSilent` — replaces the old
  "warn on every cache" test; cold miss is normal

## v0.5.4 — 2026-06-04

### Feature — pipeline services now work in isolated mode

v0.5.0–v0.5.3 fail-fasted on any job declaring `services:` in
isolated mode, on the assumption services were load-bearing
enough to deserve explicit refusal. The assumption was wrong:
services run as STANDALONE pods via `Engine.EnsureServices` and
don't share any workspace with the job pod. The only
integration point is the task pod's `HostAliases`, which gets
the service name → service Pod IP mapping — same plumbing as
shared mode.

`executeIsolated` now calls `startServices`, plumbs
`servicesPhase.hostAliases` into `IsolatedJobSpec.HostAliases`,
and defers `servicesPhase.cleanup` (a noop — services are
run-scoped, torn down by the server's `CleanupRunServices`
broadcast on run-terminal).

Operators on v0.5.0–v0.5.3 with `services:` jobs were forced to
flip back to `accessMode: ReadWriteMany`; that workaround is no
longer needed.

The dedicated rejection test
(`TestExecute_Isolated_RejectsServices`) is removed since the
rejection it asserted no longer exists.

## v0.5.3 — 2026-06-04

### Fix — artifact upload tar uses scriptWorkDir, not PVC mount root

Companion to v0.5.2's mount-path split: `PostJob`'s
`PodWorkDir` was still wired to `cfg.WorkspaceMountPath` (the
PVC root, `/workspace`). Artifact + cache paths in pipeline
YAML are relative to the SCRIPT working dir (= scriptWorkDir,
post-`target_dir` resolution), matching shared mode's
`uploader.Upload(ctx, scriptWorkDir, …)` contract. Using the
mount root made the agent exec `tar -czf - -C /workspace --
packages/types/src/generated/` which failed exit 1 because the
real path was `/workspace/src/<hash>/packages/types/src/generated/`.

Fix: pass `scriptWorkDir` as `PodWorkDir`. Same value that
already drives the task container's `WorkingDir`.

## v0.5.2 — 2026-06-04

### Fix — separate workspace mount path from task WorkingDir in isolated pods

v0.5.1 propagated the first checkout's `target_dir` into
`IsolatedJobSpec.WorkDir` so the task container's `WorkingDir`
matched where prep cloned. BuildIsolatedJobPodSpec then used the
SAME `workDir` value for:

- the workspace volume's `MountPath` on every container
- the prep init container's `--workspace` arg
- every container's `WorkingDir`

That collapsed two distinct paths (PVC root vs task CWD) into
one. With `target_dir: src/<hash>`:

- PVC mounted at `/workspace/src/<hash>` instead of `/workspace`
- Prep ran `Checkout(ctx, "/workspace/src/<hash>", co, ...)`,
  which joins `target_dir` → cloned to
  `/workspace/src/<hash>/src/<hash>`
- Task started at `/workspace/src/<hash>` → empty directory →
  plugin reported "no lockfile found", exit 2

Fix: introduce `mountPath` (= `cfg.WorkspaceMountPath`, always
the PVC root) and keep it separate from `workDir`. mountPath
goes on every `volumeMount`, on prep's `--workspace`, and on
prep/housekeeper `WorkingDir`. workDir goes only on the task
container's `WorkingDir`.

Regression test added in
`agent/internal/engine/kubernetes_isolated_test.go` —
`TestBuildIsolatedJobPodSpec_MountPathStaysAtRoot_WhenWorkDirIsSubdir`.

## v0.5.1 — 2026-06-04

### Fix — propagate `target_dir` to the task container's WorkingDir in isolated mode

v0.5.0 isolated mode hardcoded the task container's `WorkingDir`
to `WorkspaceMountPath` (`/workspace`), but the prep init
container cloned the primary material into
`/workspace/<target_dir>/`. The task started at `/workspace/`,
saw an empty directory, and exited 2 (plugins like `node`
report "no lockfile found"; bare scripts fail their first `cd`
or file read).

Fix: derive the task `WorkDir` from the first checkout's
`target_dir` exactly like shared mode does
(`runner.Execute` lines 167–177). Empty / unset `target_dir`
still falls through to `WorkspaceMountPath`, so jobs without a
material work unchanged.

Regression test added in
`agent/internal/runner/execute_isolated_test.go` —
`TestExecute_Isolated_PropagatesFirstCheckoutTargetDirToWorkDir`.

## v0.5.0 — 2026-06-04

### `agent.workspace.accessMode`: workspace isolation per job

The Kubernetes runtime now picks the workspace strategy from
`agent.workspace.accessMode`. The new default is `ReadWriteOnce`
(GHA-style isolation); the previous shared model is opt-in via
`ReadWriteMany`.

```yaml
# values.yaml
agent:
  workspace:
    accessMode: ReadWriteOnce    # NEW default; was the de-facto pre-v0.5
    storageClass: pd-ssd
    size: 20Gi
```

**ReadWriteOnce — isolated mode (new default):**

- Each job pod owns an **ephemeral PVC** via `volume.ephemeral`.
  Storage class + size from the values above; PVC dies with the
  pod.
- An **init container "prep"** runs `gocdnext-agent prep` inside
  the job pod, materialising the workspace (clone, artifact
  download) against the pod's PVC.
- The main "task" container runs the user's script/plugin.
- A "housekeeper" sidecar stays alive after the task terminates;
  the agent execs `tar -czf - <path>` inside it to stream
  artefacts to signed PUT URLs.
- Works with any CSI driver — `pd-ssd`, `local-ssd`, anything
  RWO-capable.

Why: the previous shared-PVC model required RWX storage
(Filestore/NFS) which is syscall-bound for typical artefact
patterns (pnpm `node_modules` symlink farms). On a real workload
(production Node monorepo) 83% of job time was spent untarring 60MB
of `node_modules` over NFS. Isolated mode lets operators pick a
fast block storage class.

**ReadWriteMany — shared mode (legacy, opt-in):**

Pre-v0.5 behaviour preserved unchanged. A single per-replica PVC
from the StatefulSet's VCT is mounted by the agent AND every job
pod it spawns. Required for caches and multi-task jobs (see
limitations below).

**v0.5.0 limitations in isolated mode (follow-up issues):**

- **Multi-task jobs not supported.** Pods are 1-per-job, not
  1-per-task, so we'd need init-container chaining + exit-code
  wrapping per task — deferred. Multi-task jobs in isolated mode
  fail fast with a clear error pointing to `accessMode:
  ReadWriteMany`.
- **Pipeline services not supported.** A job declaring `services:`
  (postgres/redis/etc.) is load-bearing — silently dropping the
  declarations would break the job. Fails fast with a clear error;
  use `accessMode: ReadWriteMany` for those jobs.
- **Caches are skipped.** Init container has no gRPC session to
  call `RequestCacheGet`. Job runs without pre-populated cache;
  next cold build is slower. Switch to `ReadWriteMany` if you
  rely on caches.
- **test_reports skipped.** JUnit collection runs on the agent's
  local fs in shared mode; in isolated mode the XMLs live in the
  pod's ephemeral PVC and there's no exec-side parser yet. The
  Tests tab will be empty; the job itself still succeeds/fails on
  the task exit code. Warn on declaration; switch to
  `ReadWriteMany` for per-case reporting.

**Defence-in-depth notes for isolated mode:**

- Task containers run with
  `automountServiceAccountToken: false` so the agent's SA token
  is unreachable from inside user code (defends against a future
  permissive-RBAC regression).
- The assignment Secret is explicitly deleted by the runner once
  prep terminates — the payload doesn't outlive its consumption
  window even when the Pod is kept for debugging
  (`CleanupOnFailure: false`).
- Init-startup is bounded by `StartupTimeout`: a stuck PVC bind /
  image pull / unschedulable Pod fails the job rather than
  pinning it in "running".
- The agent's exec'd `tar` uses `--` before the artifact path so
  paths starting with `-` aren't reinterpreted as flags.

**Migration:** existing deployments that worked on RWX **must**
explicitly set `agent.workspace.accessMode: ReadWriteMany` to
keep that behaviour. New deployments default to `ReadWriteOnce`.

### RBAC additions

The agent's Role now grants `pods/exec` (create, get) and
`secrets` (**create, patch, delete** — *not* `get`) verbs:

- `pods/exec` is load-bearing in isolated mode: the agent
  exec's `tar` inside the housekeeper sidecar to stream
  artefacts out to signed PUT URLs.
- `secrets` lets the agent create a per-job assignment
  Secret (serialised `JobAssignment` mounted into the prep
  init container) and patch its `ownerReference` back to
  the Pod. The absence of `get` is deliberate — the agent
  only Create/Patch/Delete'es secrets it owns and never
  needs to read another secret's content. Withholding `get`
  keeps the agent SA from being a generic
  secret-exfiltration vector if the binary is later
  compromised.

Both are scoped to the agent's release namespace.

### New `gocdnext-agent prep` subcommand

The agent binary gains a `prep` subcommand for use as the init
container entrypoint in isolated mode:

```sh
gocdnext-agent prep \
  --assignment=/etc/gocdnext/assignment.pb \
  --workspace=/workspace
```

Reads a `JobAssignment` protobuf blob (mounted via Secret),
runs checkout + artifact download against the given workspace,
logs progress to stdout. Operators shouldn't need to invoke it
manually — the engine wires the init container automatically
when `accessMode: ReadWriteOnce`.

### Helm values: new agent.workspace.* fields

- `agent.workspace.accessMode`: `ReadWriteOnce` (default) or
  `ReadWriteMany`.
- `agent.workspace.housekeeperImage`: override the sidecar
  image used in isolated mode (default `alpine:3.19`, must
  have `sh` + `tar` — busybox-derived works).
- `agent.workspace.rootOverride`: rarely needed — force a
  different `GOCDNEXT_WORKSPACE_ROOT` on the agent process.

### Agent env additions

`GOCDNEXT_K8S_WORKSPACE_MODE`, `GOCDNEXT_K8S_WORKSPACE_STORAGE_CLASS`,
`GOCDNEXT_K8S_WORKSPACE_SIZE`, `GOCDNEXT_K8S_AGENT_IMAGE`,
`GOCDNEXT_K8S_HOUSEKEEPER_IMAGE` — all wired by the chart, no
operator action needed in standard deploys.

## v0.4.39 — 2026-06-04

Two plugin changes — node v2 rewrite (breaking) and a new
gitleaks `allowlist-paths:` input (additive).

### `plugin-gitleaks`: `allowlist-paths:` input

New input for inline path-substring allowlisting without
committing a `.gitleaks.toml` to the repo. Comma- or
whitespace-separated list — each entry becomes a `.*<path>.*`
regex under `[allowlist].paths` in a runtime gitleaks config
the plugin synthesises and passes via `--config`.

```yaml
uses: gocdnext/gitleaks@v1
with:
  allowlist-paths: docs/, tests/, __tests__/fixtures/
```

Behaviour:

- **Combines with `config:`**: if the operator already supplies
  a `.gitleaks.toml`, the runtime config chains via
  `[extend].path` — operator's rules + allowlists stay active,
  ours append.
- **Default ruleset preserved**: without an operator `config:`,
  the runtime explicitly sets `[extend].useDefault = true` so
  the built-in gitleaks ruleset isn't accidentally disabled.
- **Validation**: charset restricted to `[a-zA-Z0-9/_.-]`,
  `..` and absolute paths rejected at parse time. Bad input
  exits 2 BEFORE gitleaks runs (no silent skip).
- **Composition note**: the plugin treats each path as a
  literal substring match (regex meta in input is rejected).
  Operators wanting real regex use `config: .gitleaks.toml`
  with its native `[allowlist]` block — same TOML works
  locally via `gitleaks detect --config`.

Safety reminder in the plugin manifest: every allowlisted
path is a place secrets can hide undetected. Prefer narrow
targets (`tests/fixtures/`) over broad ones (`tests/`);
review the list periodically. The feature is opt-in by
design — gitleaks defaults remain "scan everything".

### **BREAKING: `plugin-node` v2 rewrite.** Mirrors the python plugin's
contract — `install:` knob + shell-eval `command:` — to fix three
gaps in v1: no shell encoding (`&&`/pipes), no install/run
separation, single-manager-only (pnpm).

### New input schema

| Input | v1 | v2 default | Notes |
|---|---|---|---|
| `command` | required, prefixed with `pnpm`, word-split | optional, **shell-eval via `bash -lc`**, NOT prefixed | `&&`, pipes, redirects, env expansion all work |
| `install` | implicit (operator runs `command: install --frozen-lockfile`) | **`true`** (auto pnpm install before command) | `false` for downstream jobs consuming artifact |
| `manager` | implicit (pnpm only) | `auto` (detects from lockfile) | `pnpm` / `npm` / `yarn` (v3+) / `none` |
| `frozen` | implicit | `true` | maps to `--frozen-lockfile` / `npm ci` / `--immutable` per manager |
| `prod` | not available | `false` | skip dev deps for production builds |
| `working-dir` | unchanged | `.` | same as v1 |

### Migration

| v1 YAML | v2 YAML |
|---|---|
| `command: install --frozen-lockfile` | (drop — defaults install:true frozen:true do this) |
| `command: --filter @web lint` | `command: pnpm --filter @web lint` |
| `command: exec tsc --noEmit` | `install: false` + `command: pnpm exec tsc --noEmit` (downstream of an install job) |
| `command: test --run` | `command: pnpm test --run` |

### Why breaking instead of v2 paralelo

Greenfield: zero external users of plugin-node@v1 outside the
internal dogfood pipelines (gocdnext's own ci-web + one production
consumer). The maintenance cost of two parallel images
(documented, tested, rebuilt on every release) outweighs the migration
cost (one PR per consuming pipeline).

The `:v1` rolling tag now points at the new v2 image — operators on
`@v1` get the breaking change on next pull. Pin to `:0.4.38` if you
need v1 behaviour until you finish migrating.

### Yarn v1 explicitly rejected

Yarn classic (v1) has been maintenance-only since 2022 and uses a
different install-flag dialect from v3+. Supporting both doubled the
test matrix for ~zero modern users. Pipelines pointing at a `yarn.lock`
without `.yarnrc.yml` (v1 signal) get a clear error with three options:
upgrade to yarn v3+, switch to pnpm/npm, or use `manager: none` and
invoke yarn directly via `command:`.

### Multi-manager auto-detection

Priority: `pnpm-lock.yaml` > `yarn.lock` (with `.yarnrc.yml` ⇒ v3+,
without ⇒ v1 rejected) > `package-lock.json`. Override via
`manager: pnpm|npm|yarn|none`. `none` skips install + setup entirely
for jobs that run plain `node script.js` or use pre-built tooling.

### Cache paths per manager

Plugin redirects each manager's store/cache to a workspace-relative
path so the platform's `cache:` block can tar it:

| Manager | Cache path |
|---|---|
| pnpm | `.pnpm-store/` |
| npm | `.npm-cache/` |
| yarn v3+ | `.yarn/cache/` (default) |

### Dockerfile

Added `bash` to the runtime image (alpine ships `ash` only;
`bash -lc` is required for shell-eval of `command:`).

### Dogfood

`.gocdnext/ci-web.yaml` migrated to v2 contract as the reference
example: one install job + three `install: false` downstream jobs.

## v0.4.38 — 2026-06-04

Patch release fixing one bug in the `python` plugin.

### `python` plugin: rewrite_venv_shebangs now catches uv exec-wrapper

`uv sync` generates two flavours of entry-point script in `.venv/bin`:

1. **Classic shebang** — `#!/path/to/.venv/bin/python` on line 1.
2. **Exec-wrapper trick** — `#!/bin/sh` on line 1 (generic) with
   `'''exec' "/path/to/.venv/bin/python3" "$0" "$@"` on line 2.

The plugin's `rewrite_venv_shebangs` only touched line 1, so an
artifact-restored venv with the line-2 wrapper survived the plugin's
cross-job rewrite logically but still pointed at the install job's
dead workspace path at runtime. Result: `uv run mypy app/` produced
`.venv/bin/mypy: 2: exec: /workspace/<install-uuid>/.../python3: not found`
even with `no-install: true` set.

Fix: discover the OLD venv root by reading
`export VIRTUAL_ENV=...` out of `.venv/bin/activate` (every manager
writes it verbatim at create time), then `sed -i 's|old|new|g'`
globally across every regular file under `.venv/bin/` AND across
the activate variants themselves. Catches both shebang flavours
plus any other absolute reference the wrappers carry. Idempotent,
manager-agnostic.

Bumps plugin image `ghcr.io/klinux/gocdnext-plugin-python:v1` to
include the fix — operators on `v1` get the fix on next pull;
those pinning a specific tag need to bump to `:v0.4.38`.

## v0.4.37 — 2026-06-02

Cache key templating with `{{ hash "..." }}` ([issue #5](https://github.com/klinux/gocdnext/issues/5))
and an operational audit script for stuck cyclic-needs runs
([issue #6](https://github.com/klinux/gocdnext/issues/6)).

### Cache key templating

Before this release, `cache.key` was a literal string. Invalidating
a node_modules cache on lockfile change required either a constant
key (relying on `pnpm install --frozen-lockfile` to absorb drift —
fragile) or editing the YAML on every dep bump. GitHub Actions,
CircleCI, Drone, Bitbucket Pipelines all expose `{{ hashFiles }}`
templates; this lands the same shape with a closed grammar.

Syntax:

```yaml
caches:
  - key: pnpm-nm-{{ hash "pnpm-lock.yaml" }}
    paths: [node_modules, apps/*/node_modules, packages/*/node_modules]

  - key: docker-{{ hash "Dockerfile" }}-{{ hash "go.sum" }}
    paths: [/var/cache/docker]
```

Function whitelist (v1): `hash "<literal path-or-glob>"` returning
12 hex chars. `env`, `git.rev`, `format`, etc. are intentionally
deferred — each new function expands the audit surface and we want
the grammar to grow under PR review, not by accident.

Security posture (per CLAUDE.md):

- **Single-pass**: function output is hex `[0-9a-f]{12}`, which
  cannot match template syntax — chain expansion is structurally
  impossible.
- **Args are literal-only**: parser rejects non-quoted arguments,
  variable references, and nested `{{ }}`. No template engine
  inside template arguments.
- **Bounded parsing**: max 1024-char raw template, max 5 tokens,
  max 255-char arg, max 100-file glob expansion, 16 MiB
  per-file + 64 MiB total cap on `hash()` byte intake. Regex
  pre-compiled with quantifiers bounded by input cap.
- **Path traversal blocked**: `..` and absolute paths rejected at
  parse time so the agent's resolver never sees them.
- **Symlinks rejected**: agent's hash resolver `lstat`s each
  match and refuses non-regular files. A repo can't point a
  declared lockfile at `/etc/passwd` and have the agent fold
  its content into a cache key digest.
- **Charset enforced at PARSE time for TOKENIZED keys only**:
  when a key contains `{{ ... }}`, every literal chunk must
  match `[a-zA-Z0-9-_.]`. Zero-token (legacy) keys remain opaque
  — `pnpm-store-${CI_COMMIT_BRANCH}`, paths with `/`, dot-style
  versions all keep working as before. Storage hashes the raw
  key via SHA-256, so legacy chars never reach storage paths;
  the strict charset is a NEW contract opted into by writing a
  template. Mixing shell-substitution with templates
  (`pnpm-${X}-{{ hash "y" }}`) is rejected — pick one model.
- **Cancel propagation**: `expandCacheKeys` threads `ctx` into
  the resolver; `CancelJob` aborts a mid-hash read at the next
  64 KiB chunk boundary instead of blocking until EOF.

Server/agent split:

- **`proto/cachekey`**: shared parser package (both sides import).
  Compiles the template once, exposes `Parse` + `Expand`.
- **Server**: `parser.toJob` validates every `cache.key` at apply
  time. Bad config fails the project apply, not the run dispatch.
- **Agent**: `runner.expandCacheKeys` runs AFTER checkout +
  artifact downloads, BEFORE `fetchCaches`. Reads workspace files,
  glob-expands and hashes deterministically (sorted match order,
  content + relative-path folded into sha256), stamps the
  expanded key onto the proto in place.

Backwards-compat: keys with no `{{` tokens take the no-op fast
path — zero behaviour change for every pre-v0.4.37 key, including
documented forms like `pnpm-store-${CI_COMMIT_BRANCH}`,
`docker-images-${CI_COMMIT_SHA}`, paths with `/`, etc. The strict
parse-time charset is a NEW rule operators opt into by writing a
`{{ ... }}` template; existing pipelines upgrade with no edits.

### Operational audit script

`scripts/audits/stuck_runs_cyclic_needs.sql` ships a one-shot
query operators can run to detect runs in `queued`/`running > 1h` because
of a `needs:` cycle baked into the snapshot BEFORE v0.4.36's
parser-side cycle detection. Tier 1 catches 2-cycles cheaply (the
common case); Tier 2 (commented-out recursive CTE) handles N-cycle
when the cheap query is empty but runs remain stuck. Read-only;
fixes go through the normal `CancelRun` path.

### Tests

- `proto/cachekey/parser_test.go`: 31 cases covering happy path,
  every limit, every malformed-template class, path-traversal
  rejection, expansion determinism, charset enforcement on
  tokenized keys, legacy-literal passthrough, and the
  shell-substitution-vs-template-mix rejection.
- `agent/internal/runner/cachekey_expand_test.go`: 13 cases for
  the workspace resolver — determinism, content-sensitivity,
  rename-detection, glob ordering, zero-match + over-limit
  rejections, ctx cancel propagation, per-file byte cap,
  symlink rejection (leaf-in-workspace AND directory-chain
  escape both via single-file and glob patterns), plus a
  sha256 recipe pin so a future refactor of the digest
  construction fails loud.

## v0.4.36 — 2026-06-02

Scheduler honours job-level `needs:` so same-stage jobs declaring
inter-job dependencies dispatch in order. The bug surfaced on a
real dogfood pipeline: `build` declaring
`needs: [eslint, typecheck, unit, types-generate]` would dispatch
in parallel with its upstreams, hit `no ready artefacts from job
"types-generate"` during `resolveArtifactDeps`, and get marked
`failed` permanently. The SQL comment at scheduler.sql:87 had
flagged "scheduler does needs-satisfaction checking in Go" as a
TODO since v0.x; it was never implemented.

5 commits, 5 review rounds. Final shape:

### Dispatch-time gate

- New lean SQL projection `ListJobStatusForRun (name, matrix_key,
  status)` loaded ONCE per dispatch tick. Folded into a name-keyed
  map; the gate consults it per candidate.
- `needsSatisfied()` returns `Ok / UpstreamTerminal / Detail`.
  Matrix fanouts require ALL children green. Short-circuit on the
  first blocker so the operator sees the most relevant signal.
- Gate runs BEFORE agent / secrets / artifact lookups so a
  blocked job doesn't consume a session slot.
- Non-terminal upstream (queued / running / awaiting_approval):
  leave queued; next NOTIFY-driven tick re-evaluates with fresh
  status. Terminal non-success: mark downstream `failed` via
  `FailJobWithReason` (see below).

### Silent-green closure

A `needs` snapshot drift (older parser, schema change, manual DB
poke) could otherwise produce a runtime needs-unmet → downstream
`skipped` → stage / run cascade ignores `skipped` (only counts
`failed`) → run finalizes as `success` despite a job that never
ran. Renamed `SkipJobRunWithReason` → `FailJobRunWithReason`,
setting `status='failed'` so the cascade counts it. The `error`
column carries `"needs unmet: <upstream>: <status>"` for audit.
Notification-trigger skips (`SkipJobRun`) stay `skipped` — there
the "by design, never going to run" semantic differs from
needs-cascade.

### Parser validation

`validateNeeds` rejects three classes at apply time so the
scheduler doesn't have to defend at dispatch:

- Unknown name (`needs: [ghost]`) — would silently skip downstream
  and finalize run green; closed at parse.
- Self-reference (`needs: [self]`) — pointless self-wait.
- Forward-stage reference — would deadlock (later stage never
  starts because earlier stage never closes).

`validateNoCycles` adds DFS three-color cycle detection for
same-stage 2-cycle, 3-cycle, and larger cycles that
forward-stage rejection misses. Error message traces the cycle
path deterministically (alphabetical visit order for stable
output across CI reruns).

### Wake-on-completion

`NotifyRunQueued` now fires on every non-terminal job completion
(was: only when stage closes). Same-stage `needs:` siblings used
to wait up to the periodic 15s tick because the stage stayed
open while the gated downstream was queued. NOTIFY is
microseconds; the dispatch handler is a no-op when there's no
eligible work.

### Performance

Migration 00038 adds covering index
`job_runs (run_id, name, matrix_key NULLS FIRST) INCLUDE (status)`.
Without it every dispatch tick paid a seq scan over cumulative
history. Built CONCURRENTLY with `-- +goose NO TRANSACTION` so
the migration doesn't block agent writes during deploy.
Idempotent on retry: `DROP INDEX CONCURRENTLY IF EXISTS` runs
before the CREATE so a prior partial failure (leaving the index
INVALID) is cleaned up before rebuild — `CREATE … IF NOT EXISTS`
alone matches by name, not state, and would silently leave the
unusable index in place.

### Defense-in-depth

- `clampNeedsField` (128 bytes per field) applied to
  `describeBlocker` AND the missing-dep / no-rows paths in
  `needsSatisfied` + `summarizeNeeds`. Parser doesn't bound job
  names today; a 1 MiB YAML name shouldn't blow up
  job_runs.error or structured logs.
- Integration test `TestDispatchRun_NeedsGate_FailsRunOnGhostUpstream`
  bypasses the parser by writing `needs: ['ghost-job']` directly
  into a job_runs row, then proves the cascade STILL closes the
  silent-green path. Locks the defense in test code.

### Known limitation

Snapshots persisted before this release with cyclic `needs:` (no
parser-side validation at apply time then) can still hang at
runtime. Tracking issue filed for an operational health-check
query. Not a blocker on the path forward — the parser now rejects
new occurrences and the runtime gate handles non-cyclic cases.

## v0.4.35 — 2026-06-01

Run-scoped Kubernetes services (one pod per run vs per-job leak),
Woodpecker-style per-operation timings in the log viewer, a
bounded-and-coalescing cleanup-worker subsystem on the agent with
async server-side ack, and the session_generation reaper fence on
the server.

### Run-scoped k8s service pods

Previously each job that referenced `services:` brought up its own
`postgres`/`redis`/etc. sidecar pod, so a 5-job pipeline with one
postgres service produced 5 postgres pods that all leaked when the
run finished. The agent now keys service pods by `runID` (not
`jobID`) and uses a label selector + `assertOurServicePod` to reuse
the existing pod across jobs of the same run.

- **Pod naming**: `gocdnext-svc-<runShort>-<svcName>`; full label
  tuple `managed-by=gocdnext-agent`, `component=service`,
  `run-id=<runID>`, `service=<name>`.
- **Cleanup**: new `Engine.CleanupRunServices(ctx, runID)` method
  on the agent's engine interface. The server broadcasts a
  `CleanupRunServices` message at run-terminal, filtered to
  k8s-capable agents only (SQL filter on `engine='kubernetes'`
  plus in-memory `Session.Engine`).
- **`runs.has_services` snapshot** (migration 00036) computed at
  insert time from the parsed pipeline definition. Avoids the
  JSONB key-casing trap (`json.Marshal` emits `Services`, not
  `services`) and survives pipeline-row deletion.
- **`agents.engine` column** (migration 00037) persists the
  engine name reported on Register, used by the SQL filter.

### Cleanup-worker subsystem (agent)

The cleanup dispatch landed on the agent through 15 review rounds.
Final shape:

- **Bounded queue + coalesce**: 256-cap channel + per-runID
  pending set, so N broadcasts for the same run collapse to 1
  backlog slot. 4-worker pool caps concurrent k8s API pressure.
- **Process-lifetime workers**: started in `Run()` rather than
  per-stream, so a future in-process reconnect (today the
  supervisor restarts on disconnect) would not drop backlog.
- **Shutdown semantics**: shared `drainBudget` ctx (30s) installed
  in `Run()`'s defer BEFORE `cancelWorkers()`. Workers in drain
  mode derive per-item ctxs from this so the global wall-clock is
  bounded — items popped after the budget fires stay on the
  channel (single-shot abandonment audit Warn reports queued +
  pending totals after `Wait()`).
- **Race recovery**: when Go's `select` picks the queue arm with
  `ctx` already cancelled, `processShutdownRaceItem` uses the
  drain-budget parent and drains the rest, matching what the
  ctx-Done arm would have done.
- **Async ack**: new `AgentMessage.CleanupRunServicesResult`
  (oneof field 6) carries `{run_id, deleted, error_message,
  engine}` back to the server. Non-blocking send via a separate
  `cleanupAckSend` bridge — never backpressures cleanup workers
  even on a congested outbound channel; drops are reported
  periodically + on stream shutdown.

### Cleanup ack handler (server)

`handleCleanupRunServicesResult` is pure observability — no DB
writes — but with hardened validation so a buggy or compromised
agent can't poison the audit log:

- `uuid.Parse` on `run_id`; malformed payloads dropped at Warn
  with `clampBytes(64)` on the raw value.
- `clampBytes` on `engine` (64 B) and `error_message` (4 KiB).
- `deleted < 0` clamped to 0 and Warn'd explicitly.
- `sess.revoked` drop policy matches Log/TestResults paths.
- Engine self-report vs `Session.Engine` mismatch fires a Warn
  tripwire (proto comment markets this as the misconfiguration
  signal).
- `ErrSessionBusy` on dispatch loop logs at Warn (was Debug, so
  silent in prod when an agent's send queue saturated).

### Per-operation timings in logs (web)

Woodpecker-style cumulative-elapsed-since-job-start in the right
margin of the log viewer. `formatElapsed` lives in
`web/components/runs/log-viewer.tsx`; `JobCard` and
`JobDetailSheet` thread `jobStartedAt` through.

### Session generation reaper fence (server)

- **`agents.session_generation`** counter bumped atomically in
  `UpdateAgentOnRegister`. Captured at register time, returned via
  RPC, kept in the in-memory `Session.generation` (immutable after
  `CreateSession`). The reaper observes the counter at SELECT and
  fences via `FenceStaleSession(agentID, observedGen)`. Why a
  counter and not the session UUID: a DB backup containing
  session IDs would leak bearer credentials; a monotonic int
  carries the epoch signal with zero auth power.
- **`MarkAgentOffline`** is now generation-CAS so a zombie
  stream's deferred offline-mark no-ops once a successor Register
  has bumped the counter.

### Operator visibility on queued runs (issue #4 follow-through)

- `OtherRunningRunForPipeline` replaces the boolean-returning
  predecessor existence check: returns the in-flight run's id so
  the scheduler can stamp `runs.queue_reason` ("waiting on #N")
  for the runs-list UI.
- `ClearRunQueueReason` is idempotent and also fires on
  terminal-cancel paths so a canceled-while-queued run doesn't
  carry a stale waiting-on message.

### Misc

- `UnassignJob` snapshot-CAS on `(agent_id, attempt)` so a
  dispatch failure rolling back doesn't clobber a reaper-requeued
  row. Attempt is NOT bumped (dispatch never reached the agent =
  not a failed attempt).
- `clampBytes` constant trio added on the server cleanup-ack path:
  `cleanupAckRunIDMax=64`, `cleanupAckEngineMax=64`,
  `cleanupAckErrorMax=4 KiB`.

## v0.4.34 — 2026-06-01

Closes [issue #3](https://github.com/klinux/gocdnext/issues/3) —
duplicate artifact rows for the same path on a single run + the
downstream consumer getting `no ready artefacts from job "X"
matching paths [...]` even though the UI showed the artifacts as
ready.

### Root cause

`requeueStaleJob` (reaper / register-fence path, v0.4.32) and
`RerunJob` deleted `log_lines` and `test_results` on a retry but
left the prior attempt's **artifacts** as-is. When the new attempt
re-uploaded the same paths, two `ready` rows accumulated for the
same `(job_run_id, path)`. Same incident also explained the
"missing `artifact uploaded:` log line" — the prior attempt's
log_lines were cleared by the reclaim AFTER they were emitted.

### Fix

- **Migration 00035**: partial unique index on
  `artifacts(job_run_id, path) WHERE deleted_at IS NULL`.
  Defends the invariant at the schema layer — a future regression
  in the retire path fails loudly instead of silently producing
  duplicates again.
- **`RetireArtifactsByJobRun`** (new query + store method):
  soft-deletes every still-active artifact for a job_run
  (`deleted_at = NOW`, `status = 'deleting'`, `expires_at = NOW`).
  Mirrors `DeleteLogLinesByJob` / `DeleteTestResultsByJobRun`
  semantics — runs in the SAME transaction that bumps
  `job_runs.attempt`. After commit, `ListReadyArtifactsByRunAndJobName`
  no longer surfaces stale rows to downstream consumers, and the
  sweeper GC's the storage objects on its next pass via the
  existing 'deleting'-status branch.
- Wired into both `sweeper.requeueStaleJob` and
  `runs_actions.RerunJob`.
- **Path normalization** (`store/artifacts.go`): trailing slashes
  trimmed on `InsertPendingArtifact` AND on `ListReadyArtifactsByRunAndJob`
  so producer and consumer YAMLs can disagree on the trailing slash
  without breaking the lookup. Operator-level robustness — `dist/`
  and `dist` collapse to the same canonical key.

### Plugin cascade audit

Audited every plugin under `plugins/` for the same class of bug
that issue #2 fixed in `python` (hardcoded install step that
destroys artifact-restored state). Verdict: **only `python` had a
real bug**. `terraform` was flagged as "wasteful" by an automated
scan, but actually delegates the `init` decision to the operator's
`command:` input — no fix needed.

Plugins surveyed (all SAFE): `node`, `go`, `maven`, `gradle`,
`terraform`, `ansible`, `docker`, `helm`, `kubectl`, `aws-cli`,
`gcloud`. Pure-wrapper plugins (`slack`, `discord`, `email`,
`teams`, `gitleaks`, `trivy`) have no install step at all.

### Hardening from review (rounds 2-5)

- **Migration 00035 is now upgrade-safe** on DBs that ALREADY hit
  the bug. Two backfill steps run BEFORE the unique index is
  created: `regexp_replace` trims trailing slashes from existing
  paths (so post-upgrade lookups by `dist/` still find legacy
  `dist/` rows), and a CTE retires all-but-one duplicate per
  `(job_run_id, path)` (pinned > ready > newest wins). Idempotent
  on clean DBs.
- **`RetireArtifactsByJobRun` clears `pinned_at`** — a pinned
  artifact whose owning attempt died otherwise sat invisible to the
  lookup (`deleted_at` filter) AND skipped by the sweeper
  (`pinned_at IS NULL` guard), orphaning the storage object forever.
  Same fix applied to the `RerunJob` UPDATE.
- **Sweeper now reaps stale `pending` rows** older than the grace
  window. Closes the leak path where a gRPC drop or `SignedPutURL`
  failure mid-batch left pending rows that the partial unique index
  would then refuse to overwrite on the agent's next attempt.
- **`RequestArtifactUpload` dedupes paths and inserts atomically.**
  `dist`, `dist/`, `dist` in the same request now produce ONE
  ticket (first-occurrence shape wins for round-tripping back to
  the agent), and the per-batch insert uses
  `InsertPendingArtifactsBatch` so a mid-loop failure rolls back
  cleanly instead of leaking half a batch of pending rows.
- **Agent uploader dedupes BEFORE the RPC** (round 3). The
  server-side dedupe alone broke the agent's
  `len(tickets) == len(paths)` check: server returned 1 ticket
  for `[dist, dist/, dist]`, agent refused the response as
  malformed. Agent now dedupes by canonical form before the RPC
  AND the length check compares against the deduped count.
- **Migration backfill clears `pinned_at`** on retired duplicates
  (round 3). The runtime retire path already does this, but the
  in-migration UPDATE didn't — leaving a pinned legacy duplicate
  as `status='deleting'` / `deleted_at NOT NULL` / `pinned_at
  NOT NULL`, invisible to the lookup AND skipped by the sweeper's
  `pinned_at IS NULL` guard.
- **Migration takes `LOCK TABLE artifacts IN SHARE MODE`** (round
  4). Rolling-upgrade safety: kubernetes keeps the old pod
  serving RequestArtifactUpload until the new pod passes readiness,
  so without the lock an old-pod insert between dedupe and
  CREATE UNIQUE INDEX could plant a fresh duplicate the index then
  refuses, trapping the operator in a half-upgraded cluster. SHARE
  blocks writes (queued, not failed) while letting reads through;
  the window is sub-second on realistic deployments.
- **Parser dedupes artifact paths by canonical form** (round 4).
  `paths: [dist, dist/]` + `optional: [dist/, screenshots]` used to
  produce a job assignment with both `dist` and `dist/` in
  ArtifactPaths — agent-side dedupe collapsed the required batch,
  but the optional batch then tried to insert `dist/` (which the
  store canonicalizes to `dist`), hit the unique index, the txn
  rolled back, and `screenshots` was lost as collateral. Parser
  dedupe means the wire shape carries a clean (canonical-unique,
  cross-list-deduped) separation before the agent ever sees it.
- **`BuildAssignment` also dedupes at dispatch** (round 5). The
  parser dedupe runs at `apply` time, but `run.Definition` is the
  persisted snapshot from whatever release applied the pipeline.
  Pre-fix pipelines living in the DB before upgrade still carried
  the raw duplicates; dispatching them re-opened the cross-list
  collision the parser dedupe was supposed to close. Deduping in
  `BuildAssignment` covers any persisted definition regardless of
  apply-time release.

### Schema

- Partial unique index `idx_artifacts_jobrun_path_active` (00035)
  with in-migration backfill (path normalization + duplicate
  retirement).

## v0.4.33 — 2026-06-01

Closes [issue #2](https://github.com/klinux/gocdnext/issues/2) — python
plugin re-resolved the venv on every job and stripped PEP 621 extras
(ruff/mypy/pytest under `[project.optional-dependencies].dev`), making
the install-once-reuse-N pattern decorative. Three new plugin inputs:

- **`extras`** (comma/space-separated string) — `extras: dev, test`
  enables those extras at install time. uv → repeated `--extra X`,
  poetry → `--extras "X Y"`, pip → `pip install -e ".[X,Y]"` after
  the requirements file. Pipeline `with:` is `map[string]string`,
  so the value is a single string, not a YAML list.
- **`all-extras`** (bool) — uv/poetry `--all-extras`. pip has no
  equivalent; honoured as a no-op + warn so multi-manager pipelines
  don't break on the pip leg.
- **`no-install`** (bool) — skip the dependency sync, trust the
  `.venv/` already in the workspace. `rewrite_venv_shebangs` +
  `activate_venv` still run so an artifact-restored venv from an
  upstream install job is immediately usable. Manager-agnostic.

The combination closes the original symptom: a `install` job syncs
with `all-extras: true` and exposes `.venv/` as an artifact; downstream
`ruff`, `pytest`, `mypy` jobs declare `no-install: true` and consume
the venv without re-resolving. No more `--all-extras` workarounds in
every `uv run` command, and the artifact transfer actually saves work.

Refuses `all-extras: true` + `extras: [...]` together (ambiguous) and
refuses `no-install: true` when `.venv/` is missing (with a hint to
add it to `needs_artifacts`).

Doc additions in `plugins/python/plugin.yaml`:
- new example "install once, reuse across jobs" demonstrating the
  fan-out pattern with `all-extras` + `no-install`.
- new example "install with explicit extras (not all)" for the
  `extras: dev, test` variant.

## v0.4.32 — 2026-06-01

Closes [issue #4](https://github.com/klinux/gocdnext/issues/4) — operator
visibility: "I pushed a commit and the runs tab shows nothing / a
phantom-running pipeline blocks all subsequent pushes". Three distinct
silent paths fixed; the schema and control-plane invariants around the
register/dispatch/reaper cycle hardened against the data corruption the
operator-visibility gap was masking.

### HIGH — stuck-running rows & their cascade

The reaper's `INNER JOIN agents` made NULL-agent `running` rows invisible
forever; combined with the serial-concurrency gate's silent "leaving
queued" log, a single phantom-running job_run permanently froze every
subsequent push on the same pipeline. Fix is a stack of small invariants
that close every overtaking-race we could find:

- `ListStaleRunningJobs` switched to `LEFT JOIN` + a second category
  catching `agent_id IS NULL AND (started_at IS NULL OR < staleness)`
  rows. Manual DB scrub or future regressions that null agent_id no
  longer create unreapable phantoms.
- **Register-fence**: when an agent re-Registers (k8s pod restart, OOM
  + supervisor retry), `ReclaimAgentJobs` requeues every still-running
  row attributed to it BEFORE the new session is published. Without
  this, the prior process's MarkAgentSeen kept the row fresh and the
  reaper skipped it forever. Snapshot CAS (`expected_agent_id`,
  `expected_attempt`) prevents the fence from clobbering healthy rows
  that already moved on.
- **Reaper-fence**: ReclaimStaleJobs now uses `notify=false`,
  `Reaper.Sweep` revokes affected agents' in-memory sessions, THEN
  fires the coalesced NotifyRunQueued. Without the fence ordering, the
  scheduler could wake on the NOTIFY and redispatch the just-requeued
  job to the SAME stale session.
- **`session_generation`** (new `agents` column, migration 00033): a
  per-agent monotonic int set on every Register, captured into the
  in-memory `Session` at construction time and used as CAS predicate
  by `MarkAgentOffline` AND `FenceStaleSession`. Defends against the
  three subtle races: (a) old defer's offline mark clobbering a
  successor's online row, (b) reaper revoking a freshly-registered
  successor session, (c) data race on `Session.Generation` (field now
  unexported + immutable post-construction).
- **Snapshot-CAS on every state-changing path**: `CompleteJob`,
  `ReclaimJobForRetry`, `FailStaleJobAtMax`, `WriteTestResults`, new
  `BulkInsertLogLinesForJob`, new `RecordAssignmentCAS` on dispatch.
  A late JobResult / log / test-results batch from a revoked session
  whose job has been redispatched will fail the CAS instead of
  corrupting the new attempt's row.
- **Log batcher**: captures `(attempt)` at receive-time, groups flush
  by `(jobID, attempt)`, calls `BulkInsertLogLinesForJob` per group.
  Fast-finishing jobs no longer lose their tail when `ClearAssignment`
  fires between push and flush.
- **`test_results` cleared on retry/rerun**: matches log-line
  semantics so a retried job doesn't surface the prior attempt's
  results in the Tests tab.

### Operator-visibility surfaces (the original issue)

- **`runs.queue_reason`** (new column, migration 00034): when the
  serial-concurrency gate fires, the scheduler stamps
  `serial-busy:<predecessor-run-id>` on the queued run. Exposed in
  the run-detail and run-list APIs as `queue_reason`. UI can render
  "waiting on #N" instead of a status-only badge.
- **Webhook fan-out logs dedup**: when `InsertModification` finds an
  existing row and skips run creation, fanout now logs Info with
  `pipeline_id`, `delivery`, `revision`, `branch`. Resolves "I pushed
  and nothing happened" being grep-invisible.

### Other observability

- Reaper logs `fenced`, `fence_no_session`, `fence_skipped_generation_changed`
  counters per sweep. New `FenceResult` enum distinguishes the three
  outcomes — "no session" (stale process already gone) is fundamentally
  different from "generation changed" (successor raced ahead).
- Partial index `idx_job_runs_running_agent` (migration 00032) for
  the fence hot path.

### Schema

- `agents.session_generation BIGINT NOT NULL DEFAULT 0` (00033)
- `runs.queue_reason TEXT` (00034)
- Partial index on `job_runs (agent_id) WHERE status='running'` (00032)

### Notes

- 12 rounds of adversarial review went into this cut. See PR for the
  per-round race walk if you want the full archaeology.
- SSE log-tail vs DB-persistence: the receive-time gate closes the
  big window (stale session pushing after revoke), but a small window
  remains between SSE publish and the batcher's CAS flush — tailers
  may briefly see lines the DB later drops via `ErrSnapshotStale`.
  Closing it completely would mean publishing only post-CAS
  (+200ms latency floor) or tagging events with `(attempt, generation)`
  for downstream filtering — deliberately deferred.

## v0.4.31 — 2026-05-30

### Fixes

- **`Failed to spawn: ruff` STILL happened after v0.4.30.** v0.4.30
  rewrote `.venv/bin/*` script shebangs correctly, but the plugin
  still ran `source .venv/bin/activate` afterward — and the activate
  script hardcodes the venv's absolute path at install time:
  ```bash
  VIRTUAL_ENV="/install-job/.../.venv"
  PATH="$VIRTUAL_ENV/bin:$PATH"
  ```
  Sourcing that in the consumer job poisoned both env vars with
  the install job's workspace path. When the user's command was
  `uv run ruff …`, uv resolved `ruff` via `$VIRTUAL_ENV/bin/ruff`,
  hit the nonexistent install-job path, and surfaced the same
  ENOENT as before. The "VIRTUAL_ENV does not match" warning we
  kept seeing was uv telling us exactly this — we just hadn't
  acted on it.

  Replace `source .venv/bin/activate` with a small in-process
  `activate_venv` helper that does the same env mutations
  (VIRTUAL_ENV, PATH prepend, unset PYTHONHOME) but uses the
  CURRENT `$PWD/.venv` path. Three branches (uv, poetry, pip) all
  go through the helper now. Idempotent + ~3 lines + zero IO.

  Together with v0.4.30's shebang rewrite, the install → lint/test
  artifact handoff should finally just work for the uv manager.

## v0.4.30 — 2026-05-30

### Fixes

- **`Failed to spawn: ruff/mypy/...` STILL happened after v0.4.29.**
  v0.4.29 added `uv venv --relocatable` but that flag only makes
  the `activate` script portable — it does NOT change the shebangs
  of entry-point scripts (`bin/ruff`, `bin/mypy`, etc.), which pip/uv
  write at install time pointing at the venv's interpreter path:
  ```
  #!/workspace/<install-job>/.../.venv/bin/python
  ```
  When the consumer job extracted the .venv via artifact, kernel
  exec of those scripts ENOENT'd on the now-stale interpreter
  path. `uv sync` only re-installed packages whose source changed
  (the editable workspace package), so it didn't regenerate the
  scripts. The previous fix's `uv venv --relocatable` was also a
  no-op on the consumer side because `.venv` already existed (from
  artifact extract), so the `[ ! -d .venv ]` guard skipped it.

  Real fix: `rewrite_venv_shebangs` helper runs in each python
  plugin invocation after dep install. Walks `.venv/bin/*`, finds
  scripts whose first line is `#!...python...`, and substitutes
  the shebang with the consumer job's own `$PWD/.venv/bin/python`.
  Idempotent — if the shebang already matches, the substitution
  writes the same line back. Fast (~30 entry-point scripts per
  typical venv, microseconds each).

  Applies to all three branches: `uv`, `poetry`, `pip`. Same root
  cause across them. Previous v0.4.29's `--relocatable` line
  removed from the uv branch — it was misleading dead code.

## v0.4.29 — 2026-05-29

### Fixes

- **Downstream jobs failed with `Failed to spawn: ruff / No such
  file or directory` when consuming a `.venv/` artifact from an
  upstream install job.** Root cause: python venvs are
  non-relocatable by default. The install job's `uv sync` created
  `.venv/bin/ruff` (and every other entry-point script) with a
  hardcoded shebang pointing at its own workspace:
  ```
  #!/workspace/<install-job-uuid>/.../services/core/.venv/bin/python
  ```
  The artifact carried that shebang verbatim. The test/lint job
  extracted into a different `/workspace/<test-job-uuid>/...` and
  the kernel returned ENOENT trying to exec the now-stale
  interpreter path. (uv's "VIRTUAL_ENV ... will be ignored"
  warning was the first clue — it only re-installed the editable
  package, not the script shebangs.)

  Fix: plugin pre-creates the venv with `uv venv --relocatable
  .venv` before calling `uv sync`, which writes
  `#!/usr/bin/env python` shebangs. The artifact-shipped scripts
  now spawn the consumer job's interpreter correctly without any
  pipeline-side workaround. Skip when `.venv` already exists so
  a caller-provided venv isn't blown away.

  Note: this only helps the `uv` manager branch. Poetry doesn't
  expose a relocatable-venv flag; the pip branch uses `python -m
  venv` which doesn't either. For those, the right pattern stays:
  share the package-manager cache via `cache:` and let each job
  sync into its own venv (~few seconds when wheels are cached).

## v0.4.28 — 2026-05-29

### Fixes

- **Many concurrent jobs → UI stuck at "running" even after cancel.**
  The agent's outbound gRPC channel was a single 256-slot buffer
  feeding all message kinds (logs, results, heartbeats). With a
  fleet of parallel jobs spamming log lines, the buffer filled,
  `sendOutbound` blocked the producer, and the K8s engine's
  `streamLogs` goroutine wedged inside an `emit` call. With that
  goroutine wedged the engine couldn't return from `RunScript`,
  the runner never reached `sendResult`, and `cancel` could only
  flip the run row — never the jobs, because the agent never
  acknowledged the cancellation.

  Two-tier delivery policy now: LogLine messages are non-blocking
  (dropped silently when outbound is full, counted, and surfaced
  via a 30s WARN tick so operators see the back-pressure) while
  JobResult / ArtifactClaim / Progress / Pong / TestResults stay
  blocking. Buffer also grew from 256 to 4096 so genuine bursts
  don't immediately exercise the drop path.

  Dropping a log line is a bad operator UX trade — losing the
  JobResult is catastrophic. With this split a stalled server
  consumer or a particularly chatty job degrades to "missed some
  log lines" instead of "the pipeline is stuck forever".

- **Artifact extraction refused python venv symlinks** (`bin/python
  → /usr/local/bin/python3.12`), breaking the install →
  lint/test artifact handoff pattern. The blanket "no absolute
  symlinks" check was too coarse — the venv symlink is intentional
  and the consumer downstream needs it as-is to find the
  interpreter.

  Allow absolute symlinks. Defend the historical concern (tar's
  symlink-then-write CVE class — symlink `evil → /etc/passwd`
  followed by a regular file at the same path clobbers
  `/etc/passwd`) with `O_NOFOLLOW` on file opens, so a malicious
  producer's permissive symlink can't be weaponised into an
  arbitrary file write. Relative symlinks still validated to
  resolve inside the dest tree.

  Test coverage: `TestUntarGz_AllowsAbsoluteSymlinks` (venv-style
  round-trip) + `TestUntarGz_RefusesToFollowSymlinkForFileWrite`
  (CVE-class regression cover with a sentinel outside dest).

## v0.4.27 — 2026-05-29

### Fixes

- **`bash -lc "${PLUGIN_COMMAND}"` and `sh -c "${spec.Script}"`
  failed with `bash: - : invalid option` when the user-supplied
  command literal started with a dash.** Reproduction the user
  hit: a pipeline written as
  ```yaml
  with:
    command: -m uv sync --frozen
  ```
  expanded to `bash -lc "-m uv sync --frozen"`; bash's `-c` flag
  doesn't stop option parsing on the next arg, so the leading `-m`
  was interpreted as another (invalid) bash flag and the
  command-string never ran.

  Fix everywhere user-controlled command text reaches a shell `-c`:
  insert the canonical `--` end-of-options marker between `-c` and
  the command string.
  - `plugins/python/entrypoint.sh`: all four `exec bash -lc` calls
    (poetry / uv / pip / none branches).
  - `agent/internal/engine/kubernetes.go`: task container
    `Command: ["sh", "-c", "--", spec.Script]`.
  - `agent/internal/engine/docker.go`: `docker run … sh -c -- "$cmd"`.
  - `agent/internal/engine/shell.go`: `exec.CommandContext("sh",
    "-c", "--", spec.Script)`.

  With `--`, a literal `-m foo` now reaches the shell as the
  command string and fails (correctly, much more clearly) with
  `sh: line 1: -m: command not found` — actionable error for the
  YAML author instead of bash's argv-parsing confusion.

## v0.4.26 — 2026-05-29

### Fixes

- **Keycloak/OIDC login redirected to the in-cluster service URL
  after the IdP callback** (e.g. `http://gocdnext-gocdnext-server:
  8153/auth/login/keycloak?next=%2F`), unreachable from the
  user's browser. Two pages built the login href as
  `${env.GOCDNEXT_API_URL}/auth/login/<provider>` — but
  `GOCDNEXT_API_URL` is the in-cluster service hostname meant for
  SSR fetches inside the web pod, NOT for the browser. Replaced
  with a relative `/auth/login/<provider>` href in both
  `app/login/page.tsx` and the sidebar; the ingress already fronts
  both the web pod and the gocdnext-server pod under the public
  hostname (e.g. gocdnext.example.com), so the browser hits the
  right path on the right host without any env wiring. Dropped
  the now-unused `loginBase` prop from `AppSidebar` /
  `SidebarUserMenu` and the debug-only "via <provider> · <url>"
  footer text that was leaking the internal hostname.

## v0.4.25 — 2026-05-29

### Fixes

- **`gocdnext/python` plugin: uv branch STILL failed with `bash: -
  : invalid option` after v0.4.23.** uv 0.5.5 mangles `-l` even
  with the `--` separator (clap quirk we couldn't talk around).
  Replaced `uv run -- bash -lc "..."` and `poetry run -- bash -lc
  "..."` with manual venv activation (`source .venv/bin/activate`
  then `exec bash -lc`) — same pattern the pip branch has always
  used. The wrapper-vs-venv distinction was never necessary;
  removing it sidesteps the whole class of argv-mangling bugs
  across uv/poetry CLI versions.

### Features

- **Trivy plugin caches its CVE database across runs.** Default
  `TRIVY_CACHE_DIR=.cache/trivy` (PWD-relative) so a `cache:
  [{ key: trivy-db, paths: [.cache/trivy] }]` block in the pipeline
  persists the ~50 MB DB blob. Trivy still verifies freshness on
  every run (default 24h policy) — caching just turns the cold-path
  download into a HEAD-only freshness check on warm runs. New
  `skip_db_update: true` knob for fully offline / air-gapped
  runners that need to skip the HEAD too.

- **Gitleaks plugin prints findings inline instead of just the
  count.** Default `verbose: true` now passes `--verbose` so each
  leak's file:line + rule + redacted secret hits stderr as it's
  discovered. Previously the operator saw only "leaks found: 13"
  and had to dig through a separately-shipped JSON report. New
  `redact: 75` default masks 75% of the secret body in the inline
  output (leaves prefix/suffix visible for identification without
  leaving the key in plaintext). Override `redact: 0` to disable
  masking (DANGEROUS — prints the secret) or `redact: 100` to
  fully mask.

- **Log viewer renders ANSI escape codes (foreground colours +
  bold).** Tools like gitleaks/trivy/go-test emit ANSI SGR codes
  to highlight warnings (yellow), errors (red), and informational
  prefixes (gray). Previously the codes rendered as literal text
  noise (`[90m4:54PM[0m`). The viewer now parses the SGR
  sequences and maps them onto the same tailwind palette
  `classifyLine` already uses (red-500/amber-500/emerald-500/
  cyan-500/blue-500/fuchsia-500), so a tool-coloured ERR matches
  our own error tint. Scope is narrow: foreground codes 30–37 +
  90–97, bold (1), reset (0/22/39). Backgrounds, italics, blink,
  underline, and 256-colour / truecolour are silently dropped —
  they'd dilute scan-ability without adding signal. Test
  coverage in `components/runs/log-viewer.test.tsx`.

## v0.4.24 — 2026-05-29

### Fixes

- **K8s engine sent `svc.Command` into `Container.Command` instead
  of `Container.Args`, shadowing the image's ENTRYPOINT.** A
  pipeline declaring
  ```yaml
  services:
    - name: postgres
      image: postgres:16-alpine
      command: ["-c", "fsync=off"]
  ```
  failed at containerd-create time with `exec: "-c": executable
  file not found in $PATH` because the K8s API was told to run
  `-c fsync=off` as the entrypoint instead of the image's own
  `docker-entrypoint.sh -c fsync=off`. Docker engine masked this
  because `docker run image -c fsync=off` correctly appends the
  args to the image's ENTRYPOINT.

  Fix: `svc.Command` now populates `Container.Args` (the
  K8s-equivalent of Docker's CMD), leaving `Container.Command`
  empty so the image's ENTRYPOINT runs. Matches docker engine
  semantics exactly.

  Regression cover: `TestEnsureServices_CommandLandsInArgsNotCommand`
  asserts the right slot is used; the generic happy-path test now
  also fails if any service pod has a populated `Command`.

## v0.4.23 — 2026-05-29

### Fixes

- **`gocdnext/python` plugin with `manager: uv` failed with `bash: -
  : invalid option` after `uv sync` succeeded.** Root cause: `uv
  run bash -lc "${PLUGIN_COMMAND}"` lets uv consume the `-l` flag
  as one of its own (uv 0.5+ treats unknown short flags before the
  command name ambiguously), leaving bash invoked as `bash c
  "command"` — bash then complains about the bare `c` and the bare
  `-` it sees in the residual argv. Fix is the canonical
  shell-passthrough form: `uv run -- bash -lc "${PLUGIN_COMMAND}"`,
  the `--` separator makes everything after it the verbatim
  command. Same fix applied to the `poetry run` branch for
  consistency (poetry handles it today but the `--` form is
  defensive against future poetry CLI changes).

## v0.4.22 — 2026-05-29

### Fixes

- **Plugin scripts hardcoded `/workspace/` as a prefix for every
  user-supplied path, breaking every job on the Kubernetes engine.**
  Symptoms in the wild:
  - `gocdnext-python: line 28: cd: /workspace/services/core: No such
    file or directory` when `working_dir: services/core` was set.
  - `gitleaks: failed scan directory` with `stale NFS file handle`
    errors across dozens of paths because `gitleaks detect --source
    /workspace/.` was walking the entire PVC root, picking up files
    from OTHER concurrent jobs in the same namespace as their
    workspaces were torn down.

  Root cause: the Docker engine bind-mounts `spec.WorkDir` to
  `/workspace` inside the container, so `/workspace/$X` resolves
  to checkout-relative. The Kubernetes engine mounts the whole PVC
  at `/workspace` and sets `WorkingDir: /workspace/<run>/<job>/src/
  <hash>`, so `/workspace/$X` resolves to PVC-root, escaping the
  job's checkout and (worse) reading other jobs' state.

  Fix touches 29 plugin entrypoints. All hardcoded `/workspace/`
  prefixes for user-input paths (PLUGIN_PATH, PLUGIN_CONFIG,
  PLUGIN_WORKING_DIR, PLUGIN_REPORT, PLUGIN_VAR_FILE,
  PLUGIN_SETTINGS, PLUGIN_KUBECONFIG, file/dest paths in s3/nexus/
  artifactory, etc.) are dropped — the paths now resolve relative
  to the container's WorkingDir, which both engines set to the
  checkout dir. Works identically under both runtimes.

  Cache-dir defaults (`PIP_CACHE_DIR`, `GOMODCACHE`, `CARGO_HOME`,
  `MAVEN_LOCAL_REPO`, etc.) also dropped the `/workspace/` prefix
  so they sit next to the project being built, matching what a
  `cache: { path: .cache/pip }` block expects. Caches in plugins
  that have a working_dir (`python`, `rust`) had their export moved
  to AFTER `cd "${WORKING_DIR}"` so a sub-project's caches land
  next to the sub-project, not at the monorepo root.

  Plugins with a leading `cd /workspace` (go, gradle, maven, node,
  golangci-lint, buf, helm, kustomize, lighthouse-ci, release-notes,
  github-release, tag) had it removed entirely — the container's
  WorkingDir is already the right place, and `cd /workspace` was
  actively escaping into the PVC root on K8s.

  ssh plugin's `cd /workspace 2>/dev/null || true` fallback was a
  symptom of the same confusion — removed; the default no-op
  (stay in WorkingDir) is correct.

## v0.4.21 — 2026-05-29

### Features

- **Kubernetes engine now wires pipeline services as separate pods
  per service (Woodpecker-style) with `hostAliases` on the task pod
  resolving each declared service name to its pod IP.** v0.4.20
  fail-loud rejected services on the k8s engine because the runner
  was hardcoded to the docker-network path; this release plumbs a
  proper `engine.EnsureServices` contract every engine implements.
  YAML stays identical (`services: { postgres: { image: postgres:16 }
  }`) — the script reaches `postgres:5432` exactly as it does under
  the docker engine, just resolved via `/etc/hosts` (zero DNS
  latency) instead of docker DNS.

  Why pod-per-service + hostAliases rather than sidecars or k8s
  Service objects:
  - Sidecars share the pod's network namespace, which forces every
    service onto the same lifecycle as the task — a restarting
    sidecar takes the task down with it. Pod-per-service decouples
    them and matches the Woodpecker mental model.
  - K8s Service objects would require either declaring ports in the
    YAML (we don't) or wrestling with headless-Service DNS, name
    collisions across concurrent jobs in the same namespace, and
    extra RBAC for `services.create`. HostAliases on the task pod
    sidesteps all of that: no Service objects, no extra RBAC, no DNS
    lookup on the hot path.

  Implementation:
  - `engine.Engine` gains `EnsureServices(ctx, services, jobID, log)
    (ServicesWireup, error)` — docker returns `{Network: ...}`, k8s
    returns `{HostAliases: ...}`, shell errors loud, contract makes
    cleanup non-nil and safe to call even on partial startup.
  - Pod naming `gocdnext-svc-<jobshort>-<svcname>` so an operator can
    `kubectl get pods -l gocdnext.io/job=<id>` and see every backing
    pod for a job.
  - Service names validated against a strict DNS-1123 charset (max
    32 chars) to keep pod-name length under the 63-char limit and
    block argv-injection paths through pipeline YAML.
  - PodIPs collected in parallel via a `sync.WaitGroup` + buffered
    errChan with first-error-cancels-the-rest semantics — image
    pulls dominate startup, serialising waits would multiply latency
    by the number of services.
  - Cleanup uses `context.Background()` so it survives the runner's
    ctx cancel (typical reason cleanup runs) and force-deletes
    (`gracePeriodSeconds: 0`) to avoid the 30s graceful-shutdown lag
    between job end and pod-IP recycling.

  Test coverage in `agent/internal/engine/kubernetes_services_test.go`:
  noop on empty, rejects empty jobID / bad names / duplicate names /
  empty image, builds correct pods + hostAliases + labels, cleanup
  runs after caller-ctx cancel, timeout cleans up started pods.

## v0.4.20 — 2026-05-29

### Fixes

- **Webhook only triggered ONE pipeline per push when several
  shared a fingerprint.** A project with N pipelines watching the
  same (repo, branch) creates N material rows with the same
  fingerprint (materials are uniqued on `(pipeline_id, fingerprint)`,
  not on `fingerprint` alone). `FindMaterialByFingerprint` was a
  `:one LIMIT 1` query with no `ORDER BY`, so it returned an
  arbitrary single row and the other pipelines silently never
  fired. Which one "won" changed across DB resets because the heap
  scan order changed.

  The store query becomes `FindMaterialsByFingerprint :many ORDER
  BY pipeline_id` and the three webhook entry points (push,
  pull_request, multi-provider) iterate every match. Each
  pipeline's modification dedup stays in place independently —
  one bad replay on pipeline A doesn't block pipeline B's run.
  Per-pipeline run-creation errors are logged but don't fail the
  delivery; only when EVERY pipeline errors does the response go
  500 (so the provider retries).

  Wire shape changed: the 202 body now carries `runs: [{run_id,
  run_counter, pipeline_id, material_id}, ...]` instead of the
  pre-fix top-level `run_id` / `run_counter`. The pull_request
  path also pre-filters materials by the per-material
  `events: [pull_request]` opt-in so push-only pipelines stay
  push-only even when they share the base ref.

- **Services on the kubernetes engine failed with a cryptic
  `docker network create: exit status 1`.** The runner's
  `startServices` unconditionally shelled out to `docker` even
  when the agent's engine was kubernetes or shell — those have
  no docker socket in the task container. The error misled the
  operator into chasing docker / DinD wiring instead of the real
  gap (services aren't yet wired for non-docker engines).

  Pre-flight check now refuses the run with a clear
  "services: %d declared but agent engine is %q — only the docker
  engine wires services today" before any `docker` call. Proper
  kubernetes sidecar implementation is a separate PR.

## v0.4.19 — 2026-05-29

### Fixes

- **AWS credentials now reach the BuildKit container.** Tell-tale of
  the bug: `403 Forbidden, RequestID: , HostID: , api error
  Forbidden` — empty RequestID/HostID because no GCS round-trip
  ever happened. BuildKit's S3 cache backend gave up at credential
  resolution. The plugin had `AWS_ACCESS_KEY_ID` /
  `AWS_SECRET_ACCESS_KEY` in its own env (injected by the runner
  profile's `secrets:` list) but BuildKit runs in a SEPARATE
  container spawned by `docker buildx create`; nothing crossed
  the boundary except what we explicitly passed via `--driver-opt
  env.X=Y`. v0.4.16 propagated the checksum opt-out env vars;
  v0.4.19 also propagates `AWS_ACCESS_KEY_ID`,
  `AWS_SECRET_ACCESS_KEY`, and `AWS_SESSION_TOKEN` (when present)
  whenever the cache backend is `bucket`. Other cache backends
  (`registry`, `inline`) don't need AWS env and the plugin keeps
  the BuildKit env clean for them.

## v0.4.18 — 2026-05-29

### Fixes

- **`gocdnext/buildx` plugin pins a newer BuildKit when targeting
  non-AWS S3 cache.** v0.4.16 set
  `AWS_REQUEST_CHECKSUM_CALCULATION=when_required` on the BuildKit
  container via `--driver-opt env.X=Y`, but BuildKit's stable image
  `moby/buildkit:buildx-stable-1` (v0.18.x) ships an aws-sdk-go-v2
  predating the v1.30 release that learned to read those env vars —
  so the opt-out was a no-op and the GCS interop request still went
  out with checksum headers + failed signature validation. The
  plugin now auto-pins `moby/buildkit:v0.20.2` when a non-AWS
  endpoint is detected; operators can override the image
  globally via `PLUGIN_BUILDKIT_IMAGE` in `with:` (in case the
  pinned version regresses upstream).

## v0.4.17 — 2026-05-29

### Fixes

- **Runner profile edits no longer require re-typing every secret.**
  The old "full-replace semantics" contract erased any secret the
  UI didn't re-send with a fresh plaintext value, forcing the admin
  to choose between the confusing "REMOVED because…" confirm and
  re-typing every credential to change one env var. The server now
  honours `__GOCDNEXT_SECRET_PRESERVE__` as a per-key value: it
  resolves the sentinel against the row's current ciphertext,
  keeping that secret untouched. Sentinels for keys that don't
  exist are dropped (covers the race where another admin deletes
  the secret between form load and save). The UI sends the
  sentinel for existing rows the admin didn't touch and drops the
  confirm dialog entirely.

- **Secret dialog blew the modal out of the viewport on long
  values.** A long single-line paste (kubeconfig, big base64) made
  the textarea expand horizontally and pushed the footer buttons
  off-screen. Added `break-all` on the textarea so unbroken strings
  wrap, `max-h-[40vh] overflow-y-auto` so very long values scroll
  inside, and a `maxLength={64 * 1024}` mirror of the server cap so
  the wire round-trip fails locally with the same shape. Dialog
  itself gains `max-h-[90vh] overflow-y-auto` for the same reason.

- **Cleaner 413 on oversized secret submit.** Server-side handler
  used to surface `MaxBytesReader` overflow as a generic
  `400 invalid json: http: request body too large`. Now it returns
  `413 secret value too large — cap is 64 KiB`.

## v0.4.16 — 2026-05-29

### Fixes

- **`gocdnext/buildx` cache against GCS / MinIO / R2 failed with
  `SignatureDoesNotMatch` (often surfaced as 403).** Recent
  aws-sdk-go-v2 (used internally by BuildKit) sends
  `x-amz-checksum-*` headers on PutObject by default; non-AWS
  S3-compatible endpoints don't recognise those headers, include
  them in the v4 canonical request, and the signature check fails.
  The plugin now pre-detects non-AWS endpoints (BACKEND=gcs/gs OR
  any custom GOCDNEXT_LAYER_CACHE_ENDPOINT) and propagates
  `AWS_REQUEST_CHECKSUM_CALCULATION=when_required` +
  `AWS_RESPONSE_CHECKSUM_VALIDATION=when_required` into the
  BuildKit container via `docker buildx create --driver-opt env.*=*`.
  Native AWS S3 cache stays on the default behaviour — checksums
  there improve integrity and AWS accepts them.

## v0.4.15 — 2026-05-29

### Features

- **`gocdnext/buildx` cache: GCS-via-interop shorthand.** BuildKit
  has no native gcs cache type, but GCS speaks S3 protocol through
  its interop endpoint. The buildx plugin now translates
  `GOCDNEXT_LAYER_CACHE_BACKEND=gcs` (or `gs`) into
  `type=s3,endpoint_url=https://storage.googleapis.com,region=auto`
  automatically. HMAC credentials must come in via the runner
  profile / job secrets as `AWS_ACCESS_KEY_ID` + `AWS_SECRET_ACCESS_KEY`
  (BuildKit reads them under those names regardless of provider).
  Missing HMAC keys fail loud at plugin boot with a clear
  "cache: gcs backend requires AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY"
  instead of producing a cryptic IMDS lookup failure inside the
  build.

## v0.4.14 — 2026-05-29

### Changes

- **Agent infers `WorkspaceRoot` from the engine choice** — v0.4.13
  added `GOCDNEXT_WORKSPACE_ROOT` as a required env, but two env
  vars that must agree (`GOCDNEXT_WORKSPACE_ROOT` and
  `GOCDNEXT_K8S_WORKSPACE_PATH`) is exactly the misconfig trap a
  default should eliminate. The agent now picks the right path on
  its own:

  | Engine | WorkspaceRoot |
  |---|---|
  | shell, docker, unset | `/tmp/gocdnext-workspace/` (runner default) |
  | kubernetes | `GOCDNEXT_K8S_WORKSPACE_PATH` (REQUIRED — boot fails loud when missing) |

  `GOCDNEXT_WORKSPACE_ROOT` stays available as an explicit override
  for operators who mount the PVC at a non-default path; the chart
  exposes it via `agent.workspace.rootOverride` and no longer sets
  it by default.

## v0.4.13 — 2026-05-29

### Fixes

- **Workspace was on the agent's local fs, invisible to job pods**
  — `runner.Config.WorkspaceRoot` defaulted to `/tmp/gocdnext-workspace/`
  on the agent pod's ephemeral disk. Job pods (the docker buildx
  plugin and any other k8s-engine task) mount the workspace PVC at
  `/workspace` but receive `WorkingDir = /tmp/gocdnext-workspace/...`
  pointing at a path that doesn't exist in their filesystem. `docker
  buildx build .` then sends an empty context to DinD and buildx
  fails with `ERROR: resolve : lstat <first-path-component>: no such
  file or directory` (the daemon sees an empty tar and can't find
  the Dockerfile's leading directory).

  The shell engine "worked" because shell tasks run on the agent
  directly via `os/exec`, so they hit the same local fs the runner
  wrote to. Any docker / k8s engine task with a non-trivial
  Dockerfile path hit the bug.

  Wires `GOCDNEXT_WORKSPACE_ROOT` env into `rpc.Config.WorkspaceRoot`;
  chart sets it to `agent.workspace.mountPath` (default `/workspace`)
  so the agent + every spawned job pod share the same PVC view of
  the cloned source. Shell-engine deployments leave it unset and
  keep the `/tmp` behaviour.

## v0.4.12 — 2026-05-29

### Fixes

- **`gocdnext/buildx` plugin** — four hardening fixes on the entrypoint:
  - **Default `PLUGIN_PLATFORMS` flips to `linux/amd64`**. Multi-arch
    via QEMU emulation on amd64 runners adds 3-5x build time and
    needs a privileged `docker run` for binfmt that
    PodSecurity-strict clusters reject. Declare
    `platforms: linux/amd64,linux/arm64` in `with:` to opt back in.
  - **Binfmt only when actually cross-building.** The plugin now
    detects host arch (`uname -m`) and skips `tonistiigi/binfmt`
    when every target platform matches the host. Saves ~15s + one
    privileged container per build on the common amd64-only case.
  - **All `PLUGIN_*` inputs trimmed.** A YAML `|` block-scalar
    leaves a trailing newline on the value (`platforms: |\n  linux/amd64\n`
    → `linux/amd64\n`), which buildx then parses as a single
    platform whose name has a trailing newline. Symptom was
    `ERROR: resolve : lstat platform: no such file or directory`
    miles away from the cause.
  - **Final `docker buildx build` invocation echoed verbatim**
    (via `printf %q`) before execution. Cryptic buildx errors now
    come with the exact argv next to them — no `set -x` ceremony
    needed to diagnose stray-whitespace inputs.

## v0.4.11 — 2026-05-29

### Features

- **CI_* built-in variables** exposed to every job — `CI_BRANCH`,
  `CI_COMMIT_SHA`, `CI_COMMIT_SHORT_SHA`, `CI_RUN_COUNTER`,
  `CI_RUN_ID`, `CI_PIPELINE_ID`, `CI_PROJECT_ID`, `CI_JOB_NAME`,
  plus the `CI=true` / `GOCDNEXT=true` markers that recipe ports
  from Drone / GitLab / Woodpecker check. Drawn from the
  `RunForDispatch` at dispatch time (deterministic across replays
  via sorted material-uuid pick), absent when the run has no
  revision so substitution can fail-fast instead of producing
  `myapp:1.7.`-style empty interpolations.

- **`${VAR}` shell-style substitution in plugin `with:` values** —
  the docs (and every plugin recipe ported from another platform)
  reference CI built-ins as `${CI_COMMIT_SHORT_SHA}`. Pre-fix, that
  literal token reached `docker buildx build` and failed with
  `invalid reference format`. Substitution is SOFT (unknown names
  pass through verbatim) so a legitimate `${HOME}` in a setting
  still gets shell-expanded at container runtime — only `${{ NAME }}`
  stays hard-fail-on-unknown.

## v0.4.10 — 2026-05-29

### Fixes

- **Plugin images cached as `:v1` never re-pulled after a release** —
  the agent's Kubernetes engine left ImagePullPolicy unset, so any
  tag except `:latest` defaulted to IfNotPresent and a node with the
  old `:v1` image cached kept serving it indefinitely after we
  cut a release. New `imagePullPolicyFor` heuristic maps tags to
  policy: moving channels (`:latest`, `:v\d+`, `:v\d+\.\d+`, `main`,
  `dev`, `nightly`, `edge`, `stable`) → PullAlways; pinned semver,
  SHA-prefixed and digest references → IfNotPresent. The plugin's
  `:v1` channel now refreshes automatically on every job; the
  agent's own `:0.4.10` immutable tag does not pay a per-job
  re-pull cost.

### Features

- **`agent.forceImagePullAlways` chart value** (env
  `GOCDNEXT_K8S_FORCE_IMAGE_PULL_ALWAYS=true`) — operator override
  that flips every task container to PullAlways regardless of the
  heuristic. Useful for clusters fronted by a registry mirror where
  HEAD is cheap and the operator wants every job to re-resolve the
  manifest, including internally-retagged "pinned" versions.

## v0.4.9 — 2026-05-28

### Fixes

- **`docker: true` was silently dropped on plugin tasks** —
  `runScript` propagated the YAML's `docker: true` flag through to
  `engine.ScriptSpec.Docker`, but `runPlugin` did not. Plugin tasks
  always ran without a DinD sidecar and without `DOCKER_HOST`, so
  every `docker` invocation inside a plugin fell back to
  `/var/run/docker.sock` (absent in the plugin's filesystem) with
  the misleading "Cannot connect to the Docker daemon" error. Now
  the plugin path mirrors `runScript`, and the agent's k8s engine
  attaches the DinD sidecar + sets `DOCKER_HOST=tcp://localhost:2375`
  whenever the job declares `docker: true`.

- **`gocdnext/buildx` plugin waited zero seconds for the daemon** —
  the entrypoint issued its first `docker run` immediately, which
  raced DinD's 1-2s startup. Now waits up to 60s for `docker info`
  to succeed; on timeout, prints a clear diagnostic mentioning
  whether `docker: true` was set so the operator knows whether DinD
  was even wired.

## v0.4.8 — 2026-05-28

Wires the `${{ NAME }}` substitution the docs have always advertised
but the code never honoured. Unblocks every plugin recipe that pulls
a secret into `with:` (buildx login, helm push token, slack webhook,
trivy registry, etc.).

### Features

- **`${{ NAME }}` reference resolution in plugin `with:` and env**
  values, against the job's declared `secrets:` first, then the
  pipeline's `variables:` map. Single-pass (no recursion), with
  identifier-only refs (`[A-Za-z_]\w*`) — Actions-style expressions
  (`secrets.X.Y`, `A && B`, function calls) deliberately fail with
  a clear "unsupported reference expression" message instead of
  silently passing through. Unresolved refs fail the dispatch with
  the reference name listed in the run error so the operator sees
  the typo at scheduler time, not as a downstream auth error from
  the plugin container. Resolved secret values land in `LogMasks`
  so they continue to be redacted from agent logs.

### Engineering

- New CLAUDE.md section **"Postura de implementação (dev sênior)"**
  codifies the corner-cases / security / performance lenses every
  PR (hotfix included) goes through. Adopted retroactively for the
  refs implementation: substitution errors never disclose
  neighbouring resolved values, regex compiled once at package
  init, fast path bypasses the regex when the input contains no
  `${{` token.

## v0.4.7 — 2026-05-28

Three fallout fixes from the v0.4.4 URL canonicalisation, all of
which surface on a real `push` → run dispatch:

### Fixes

- **`git clone` failed with exit 128 on every pipeline** — the
  implicit project material stored the canonical scheme-less URL
  (`github.com/owner/repo`), which `git clone` can't speak. New
  `domain.HTTPCloneURL` reattaches `https://` so the agent always
  sees a clonable URL. Applied in both `InjectImplicitProjectMaterial`
  (write time) and `scheduler.materialCheckouts` (dispatch time, as
  defence-in-depth for legacy material rows).

- **Webhook drift created pipelines without the implicit material**
  — `applyDrift` called `ApplyProject` directly on the parsed YAML
  without running the `injectImplicitProjectMaterial` synthesis the
  UI's `apply` and `sync` handlers ran. A config-only push that
  drove drift therefore rebuilt the pipeline rows MINUS the implicit
  "this project's repo" material, and the next push silently 202'd
  with no run. Moved the helper to `configsync` (shared package) and
  call it from all three call sites: apply, sync, drift.

- **Scm_source URL came back without a scheme in API responses** —
  the store layer canonicalised the URL on insert AND surfaced the
  scheme-less form on read, so `https://github.com/x/y` typed by the
  operator came out as `github.com/x/y` in the UI / API. Store reads
  now rehydrate via `HTTPCloneURL` so the API response carries a
  fully-qualified URL while the canonical form remains the matching
  key under the hood.

## v0.4.6 — 2026-05-28

Two more hotfixes uncovered while validating v0.4.5: plugin tasks were
silently no-op on the Kubernetes engine, and manual triggers stopped
working after the v0.4.4 URL canonicalisation.

### Fixes

- **Plugin tasks ran `sh -c ""` on the Kubernetes engine** — the pod
  spec hardcoded `Command: ["sh", "-c", spec.Script]`, so when the
  runner left Script empty for a plugin task (the image's ENTRYPOINT
  is the logic) Kubernetes overrode the entrypoint with a no-op shell.
  The container exited 0 with nothing printed, the task showed
  "success" in the run log, and no build / push / notification ever
  ran. The docker engine already handled this correctly. Now the k8s
  engine leaves Command nil when Script is empty so the image's
  ENTRYPOINT runs as authored.

- **Manual trigger 422 "no modifications yet"** — v0.4.4 changed
  scm_sources.url to the canonical scheme-less form
  (`github.com/owner/repo`). `seedHeadModification` hands that URL
  back to `github.ParseRepoURL` to mint the App token; the parser
  only handled scheme-bearing and SSH shapes, so the canonical form
  was misread (host parsed as owner) and the seed silently failed.
  Manual trigger then returned 422 because no modification existed
  yet. ParseRepoURL now recognises the canonical form too.

## v0.4.5 — 2026-05-28

Two more hotfixes — private-repo clones get an installation token, and
the pipeline overview sheet stops trying to reach localhost from the
browser.

### Fixes

- **Private-repo clones via GitHub App** — the agent failed every
  private-repo clone with `fatal: could not read Username for
  'https://github.com'` because `MaterialCheckout.url` was the bare
  repo URL with no credential. The scheduler now mints a per-repo
  installation token before dispatch (via `vcs.Registry.TokenForGitURL`)
  and embeds it as `https://x-access-token:TOKEN@host/...`. The token
  is also appended to `LogMasks` so the agent redacts it from the
  `$ git clone` echo and any error output. Public repos and SSH URLs
  fall through untouched.

- **Pipeline overview sheet's artifacts + YAML tabs hit localhost** —
  the sheet imported `env.GOCDNEXT_API_URL` directly from a Client
  Component. `process.env.GOCDNEXT_API_URL` is undefined in the
  browser bundle, so Zod defaulted the URL to `http://localhost:8153`
  and every fetch failed with "Failed to fetch". Now uses relative
  paths the same way `runs/[id]` already does after v0.4.4.

## v0.4.4 — 2026-05-28

Bug-fix release. Unblocks webhook-driven runs for any project whose
scm_source URL was registered in a different form (SSH vs HTTPS) than
the form the provider emits in its push payload, fixes the UI's
browser-side fetches when no public API URL is set, and wires the
`when.branch:` filter at the pipeline level.

### Fixes

- **Webhook fingerprint divergence (SSH ↔ HTTPS)** — `normalizeGitURL`
  now collapses `git@github.com:owner/repo` and
  `https://github.com/owner/repo` to the same canonical
  `host/owner/repo` form, so the implicit material's stored
  fingerprint matches the webhook payload's HTTPS clone_url every
  time. Pre-fix, a project bound with the SSH form received `202` from
  the webhook with `drift.applied:true` but never created the run.
  Stored URLs are now displayed in canonical scheme-less form (e.g.
  `github.com/org/repo`); re-bind affected projects after upgrade.

- **UI fetched `http://localhost:8153` in production** — the page
  baked `env.GOCDNEXT_API_URL` into client component props, which
  defaulted to localhost when the operator forgot `server.publicBase`.
  Introduced `GOCDNEXT_PUBLIC_API_URL` (optional, defaults empty);
  empty means the browser uses RELATIVE paths through the same
  ingress, which is the right default for single-host deployments.
  The chart's web Deployment now wires `GOCDNEXT_API_URL` to the
  in-cluster server service for SSR and forwards `server.publicBase`
  to `GOCDNEXT_PUBLIC_API_URL` only when set.

- **Diagnostic-friendly webhook no-match log** — the "no matching
  material" path now logs `clone_url`, `normalized_url`, `branch` and
  `fingerprint` together so the operator can diff the lookup against
  the apply-time material rows in one glance. The 202 response body
  also carries a `warning` field surfacing the same message in
  GitHub's webhook delivery viewer.

### Features

- **Pipeline-level `when.branch:` filter** — a single pipeline can
  now declare multiple tracked branches:
  ```yaml
  name: build
  when:
    branch: [main, hotfix-stable]
    event: [push]
  stages: [...]
  ```
  The apply path fans the implicit project material out into one row
  per branch so each push fingerprint (URL+branch) matches a distinct
  material row — same dispatch path as multi-explicit-material
  pipelines. Empty `when.branch:` falls back to the scm_source's
  default branch (today's behaviour).

## v0.4.3 — 2026-05-28

CI-only patch. Fixes how stable image tags are advanced so operators
pinning to `:latest` or `:v1` get the last cut release, not the
rolling main HEAD.

### Fixes

- **`:latest` and plugin `:v1` only advance on release tags** — both
  channels were gated on `is_default_branch`, so every main commit
  (not just releases) moved them. A non-release main push could land
  a half-finished feature on a tag the operator pins to. Now the
  gate is `startsWith(github.ref, 'refs/tags/v')`. main pushes still
  publish `:main` and `:sha-...` so dev consumers have a HEAD tracker.
- **Semver major tags carry the `v` prefix** — `pattern={{major}}`
  emitted a bare `0` (today) or `1` (after v1.0.0); the bare `1`
  would have silently clashed with the raw `v1` plugin-contract
  channel. Switched to `pattern=v{{major}}` and
  `pattern=v{{major}}.{{minor}}` for both core and plugin workflows.

## v0.4.2 — 2026-05-28

Chart-only patch. Restores per-replica workspace PVC mounting on
Kubernetes agents.

### Fixes

- **agent StatefulSet env-var ordering** — `GOCDNEXT_K8S_WORKSPACE_PVC`,
  `WORKSPACE_PVC_NAME` and `POD_NAME` were declared in reverse
  dependency order, so kubelet's `$(VAR)` expansion (which only
  resolves against entries DEFINED EARLIER in the list) left both
  derived values as literal strings. The agent then asked the
  scheduler to mount a PVC literally named
  `$(WORKSPACE_PVC_NAME)`. Reordered to POD_NAME → WORKSPACE_PVC_NAME
  → GOCDNEXT_K8S_WORKSPACE_PVC so each step's reference is already
  visible.

## v0.4.1 — 2026-05-28

Patch release. GitHub App now actually authenticates the
`.gocdnext/` fetch on private repos, plus a CI speedup.

### Fixes

- **GitHub App wired into the configsync fetcher** — previously the
  fetcher only consulted PAT-style credentials, so projects bound to
  a GitHub App on a private repo got a silent `404` from the Contents
  API (`config folder not found`) even with `Contents: Read` granted.
  `MultiFetcher` now mints an installation-scoped token via the App
  when no PAT is available, with the App's `apiBase` preserved so a
  GHE-bound App never has its token sent to api.github.com.
  `AppClient` caches the `(owner, repo) → installation_id` lookup and
  invalidates it on token-mint failure to recover from
  uninstall→reinstall without a server restart. A new
  `MultiFetcher.Logger` hook emits a warn on App-fallback failures
  so operators stop seeing the same "folder not found" symptom
  regardless of root cause.
- **`/settings/integrations` copy** — the GitHub App card no longer
  claims a "PAT fallback" path that the UI never exposed.

### CI

- amd64-only image builds while we stabilise — QEMU-emulated arm64
  was pacing every release at 15-25 min per image. Multi-arch
  returns when needed.
- Web job now uses pnpm with `--frozen-lockfile` and the
  `actions/setup-node` pnpm cache, matching the locally-pinned
  toolchain in `web/package.json`.

## v0.4.0 — 2026-04-29

Focused release: artifact storage configuration moves out of the
env-only swimlane and into the admin UI. Operators can now point
the control plane at a different S3 / GCS bucket without rebuilding
a Helm release; the env path stays as the boot-time fallback.

### Highlights

- **Storage backend config in the UI** — new `/settings/storage`
  tab. GET/PUT/DELETE on `/api/v1/admin/storage` back a
  filesystem / S3 / GCS picker with per-backend validation and
  AEAD-sealed credentials. The DB override wins over env when
  present; clearing the override falls back to env.
- **Generic `platform_settings` table** — one key/value/secret row
  shape that future runtime-mutable platform config (SCM defaults,
  retention overrides) reuses without a per-feature migration.
- **Restart-required surfacing** — saves return
  `X-Gocdnext-Restart-Required: true`; the UI shows an amber
  banner so the operator knows to roll the server pod. Hot-reload
  on the dispatch path is on the roadmap.
- **Audit trail for platform settings** — `platform_setting.set`
  and `platform_setting.delete` audit events record the actor +
  backend kind + credential key names for compliance review.

### Compatibility

- No breaking changes for existing deployments — the env path
  (`GOCDNEXT_ARTIFACTS_*`) keeps working unchanged. The new DB
  override is opt-in: nothing happens until an admin saves a
  config in `/settings/storage`.
- Migration `00031_platform_settings.sql` runs on boot. Forward-
  only; no destructive operation on existing data.

### Schema migrations

- `00031_platform_settings.sql` — generic key/value table for
  runtime-mutable platform configuration with AEAD-encrypted
  credentials column.

## v0.3.0 — 2026-04-30

Big release. Real-cluster smoke surfaced enough rough edges that
"helm install on a fresh cluster" now Just Works, end to end, with
no manual port-forwards, SQL inserts, or kubectl annotates. Plus a
substantial product layer: API tokens, service accounts, layer
cache, and observability landed in this cycle.

### Highlights

- **Observability** — Prometheus `/metrics` (8 series + Go runtime),
  `/readyz` with DB ping, OpenAPI 3.1 spec served at
  `/api/v1/openapi.yaml` and embedded in the binary.
- **API tokens + service accounts** — per-user tokens minted at
  `/account`, machine identities under `/admin/service-accounts`,
  Helm chart wires them through.
- **Runner profile env + encrypted secrets** — admins ship runtime
  config + AES-GCM-sealed credentials on the profile, every job
  inherits without per-pipeline plumbing. Buildx plugin gains
  `cache: registry|inline|bucket` for one-line layer caching.
- **`{{secret:NAME}}` references to global secrets** — profile
  secret values can reference globals; rotate once, propagate
  everywhere.
- **Default profile shipped via Helm** — `runnerProfiles: [default]`
  is now the chart default; pipelines reference `agent.profile:
  default` without operator pre-config.
- **Agent → StatefulSet + auto-register** — pod names are stable
  (`agent-0`), workspace is per-replica RWO, and the server
  auto-creates the DB row on first contact when the bearer token
  matches the configured registration secret. `replicas: N` Just
  Works.
- **Single-host unified routing** — one Ingress (or HTTPRoute) per
  host, server-side prefixes (`/api`, `/auth`, `/healthz`,
  `/readyz`, `/metrics`, `/version`, `/artifacts`) on the same
  hostname as the web UI. Same-origin → no CORS, OIDC and signed
  URLs work.
- **Migrations on boot** — server runs `goose up` at startup;
  no separate migration job needed.
- **EntityChip cross-surface UX** — typed pill component with
  per-entity colour + icon used on pipeline card, run banner,
  audit log target column.

### Breaking changes (chart)

- `server.ingress.*`, `web.ingress.*`, `server.gateway.*`,
  `web.gateway.*` removed. Use top-level `ingress` / `gateway`
  with `exposeServer` / `exposeWeb` toggles instead.
- Agent moved from Deployment to StatefulSet — upgraders need to
  delete the old Deployment + PVC manually before installing
  v0.3.0 (the StatefulSet's `volumeClaimTemplates` won't bind
  to the legacy shared PVC).
- `agent.workspace.accessMode` default flipped from
  `ReadWriteMany` to `ReadWriteOnce` (per-replica claim now).
- `artifacts.filesystem.accessMode` is configurable; defaults to
  `ReadWriteOnce`. The chart fail-checks at template time when
  `server.replicas > 1` + filesystem + RWO.
- `default_image` field removed from runner profile UI form
  (column kept on the row for backwards-compat). Image is a
  job/plugin concern.

### Fixes

- Postgres dev container set `PGDATA=/var/lib/postgresql/data/pgdata`
  so `lost+found` on CSI mount points doesn't break `initdb`.
- ConfigMap that ships the runner-profiles seed now mounts via
  `subPath` so it doesn't shadow the baked plugin catalogue at
  `/etc/gocdnext/plugins`.
- Web image build context changed to repo root so `docs/*.md` ship
  with the standalone server. `/docs` page is now `force-dynamic`
  so it reads markdowns at request time, not build time.
- DTO for runner profile always emits `tags: []`, never `null`.
- Plugin go: installs `gcc + musl-dev` so cgo (`go test -race`)
  works on the alpine base.
- Scalar API explorer: hosted on its own Astro page outside
  Starlight, with light/dark logo variants and a relative spec
  URL that respects the Astro `base` prefix.

## v0.2.0 — earlier

API tokens + service accounts. Approver groups with quorum.
Cache eviction policy. Pipeline services. Single-job rerun. Logo
redesign. Implicit project material. Cancel kills container.

## v0.1.0 — earlier

Initial public preview. Core pipeline + scheduler + agent.
