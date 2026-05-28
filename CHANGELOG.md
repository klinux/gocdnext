# Changelog

All notable changes to gocdnext.

The format follows [Keep a Changelog](https://keepachangelog.com/),
versions follow [SemVer](https://semver.org/) (with the v0.x.y
convention that minor bumps may carry breaking changes until 1.0).

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
