# Changelog

All notable changes to gocdnext.

The format follows [Keep a Changelog](https://keepachangelog.com/),
versions follow [SemVer](https://semver.org/) (with the v0.x.y
convention that minor bumps may carry breaking changes until 1.0).

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
