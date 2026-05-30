# Changelog

All notable changes to gocdnext.

The format follows [Keep a Changelog](https://keepachangelog.com/),
versions follow [SemVer](https://semver.org/) (with the v0.x.y
convention that minor bumps may carry breaking changes until 1.0).

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
