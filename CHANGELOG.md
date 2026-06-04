# Changelog

All notable changes to gocdnext.

The format follows [Keep a Changelog](https://keepachangelog.com/),
versions follow [SemVer](https://semver.org/) (with the v0.x.y
convention that minor bumps may carry breaking changes until 1.0).

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
inter-job dependencies dispatch in order. The bug surfaced on the
cora-pulse dogfood pipeline: `build` declaring
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
  (the editable corapulse-core), so it didn't regenerate the
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
  hostname (e.g. gocdnext.cora.tools), so the browser hits the
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
