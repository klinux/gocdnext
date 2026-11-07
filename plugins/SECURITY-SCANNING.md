# Plugin image vulnerability scanning

The gocdnext plugin images are scanned on a standing schedule by
[`.github/workflows/trivy-plugins.yml`](../.github/workflows/trivy-plugins.yml),
which runs Trivy against every **published `:v1`** plugin image plus the external
images the buildx plugin pins (binfmt + buildkit), then applies a policy against
a versioned baseline of justified exceptions
([`.trivy-baseline.yaml`](.trivy-baseline.yaml)) using the comparator
[`scripts/trivy_gate.py`](scripts/trivy_gate.py).

It runs weekly (`cron`) and on demand (`workflow_dispatch`) — it is **not** a
per-PR blocker. Its job is to catch regressions in what is already shipped and to
force stale exceptions to be renewed or removed.

**Fail-closed.** The set of targets the gate *expects* to scan comes from the
**versioned inventory** — every plugin dir minus the `unpublished:` list in the
baseline — **not** from what the registry resolves. A GHCR outage or auth error
therefore cannot shrink the scanned set to a green run: a scan that can't resolve
or pull its image fails its job, and the gate treats the missing result as a hard
failure.

## Policy

| Severity | Rule |
|---|---|
| **CRITICAL** | Fails immediately. Allowed only with a **non-expired `emergency` exception** — explicit, emergency-only, window **≤ 7 days**. |
| **HIGH, fixable by us** | Does **not** normally go in the baseline — fix it (bump base, `apk upgrade`, patch). A temporary `high-fixable` exception is **≤ 14 days** cover only. |
| **HIGH, no fix / upstream-bound** | `upstream-bound` or `no-fix` exception, **≤ 90 days**, renewed by a PR with a fresh assessment. |
| **Expired / over-window exception** | Hard fail — the comparator enforces `expires - approved_at ≤` the class window; renew (new PR) or remove. |
| **Invalid baseline** | Hard fail (schema/parse error, missing field, duplicate, bad/invalid date). |
| **Exception matching no finding** | Reported as **obsolete** (warning) — remove it to avoid silent accumulation. |
| **Missing scan for an expected target** | Hard fail — a dropped scan must never silently hide a CRITICAL. |

The class **windows (7 / 14 / 90 days) are HARD-enforced**: the comparator checks
`expires - approved_at` against the class limit, that the exception is not
expired, and that a CRITICAL is held only by an `emergency`-class entry.

### HIGH rollout (calibration)

`CRITICAL` and expired/invalid exceptions fail from day one. `HIGH` starts in
`observe` mode (`HIGH_MODE: observe` in the workflow): unbaselined HIGH findings
appear in the run summary + the `trivy-gate-report` artifact but do **not** fail
the run. After two weekly runs have been triaged into the baseline, a one-line PR
flips `HIGH_MODE` to `enforce` (`HIGH not in baseline` then fails).

## The baseline file

[`.trivy-baseline.yaml`](.trivy-baseline.yaml) is a list of **specific**
exceptions — not a blanket `.trivyignore`.

**Identity** — an exception applies only when ALL of these match a finding, so a
different version or binary never inherits it:
`(target, trivy_target, cve, package, installed, origin)`. One entry per distinct
identity; `targets:` lists every logical target that shares it.

```yaml
version: 1
# Plugin dirs that intentionally have NO published :v1 yet (don't expect a scan).
unpublished: []
exceptions:
  - targets: [docker, buildx, docker-push]       # plugin dirs, or external-<name>
    trivy_target: usr/local/libexec/docker/cli-plugins/docker-buildx  # Trivy Target
    cve: CVE-2026-17106
    package: github.com/moby/go-archive
    installed: v0.2.0            # EXACT installed version (part of identity)
    fixed_version: "0.3.0"       # "" when no fix exists (record only)
    origin: gobinary            # Trivy `Type`: gobinary | alpine | node-pkg | ...
    class: upstream-bound       # emergency | high-fixable | upstream-bound | no-fix
    justification: >-           # why it is acceptable right now
      moby/go-archive compiled into the bundled docker-buildx binary.
    mitigation: >-              # what reduces the risk / the exit plan
      Adopt the first docker CLI image shipping the patched module.
    advisory: https://avd.aquasec.com/nvd/cve-2026-17106
    upstream_issue: https://github.com/moby/go-archive
    approved_pr: 334            # PR that approved this exception
    owner: klinux               # who owns the follow-up
    approved_at: "2026-11-07"   # when approved; window = expires - approved_at
    expires: "2027-02-05"       # quoted YYYY-MM-DD; gate fails once past
```

All fields except `fixed_version` are **required**; a missing one fails the gate.
`origin` must equal Trivy's reported `Type`, and `trivy_target` its `Target`
(find both in the Trivy JSON for the finding).

### Targets

- A plugin target is its directory name under `plugins/` (e.g. `docker`).
- An external pinned image is `external-<name>` (`external-binfmt`,
  `external-buildkit`). The workflow reads the exact pinned refs from
  `plugins/buildx/entrypoint.sh`, so the gate always scans what actually ships —
  and **requires each to be `@sha256:`-digest-pinned**, so a future switch back to
  a floating tag fails discovery instead of silently scanning a moving ref.

The workflow downloads the Trivy DBs **once** and shares them as an artifact
(`--skip-db-update` on each scan), so all targets scan against one consistent
snapshot without hitting the DB registry per target.

## Working with exceptions

- **Add**: append an entry with all required fields and a justified `expires`
  within the window for its `class`. Open a PR; its number goes in `approved_pr`.
- **Renew**: a fresh PR that re-assesses and bumps `expires` (and
  `approved_pr`). Do not silently extend.
- **Remove**: when the finding is gone the gate flags the entry `obsolete`;
  delete it. When a fix ships, fix the image and remove the entry.
- **Emergency CRITICAL**: only with sign-off, `class: emergency`, `expires`
  within 7 days, and a clear mitigation + exit plan.

## Running locally

```sh
# scan an image to JSON named after its target
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock -v "$PWD/out:/out" \
  aquasec/trivy:0.69.3 image --quiet --severity HIGH,CRITICAL --scanners vuln \
  --pkg-types os,library -f json -o /out/docker.json \
  ghcr.io/klinux/gocdnext-plugin-docker:v1

# evaluate (observe mode; add --high-mode enforce to preview enforcement)
python3 plugins/scripts/trivy_gate.py \
  --baseline plugins/.trivy-baseline.yaml --results-dir out \
  --expect-targets docker

# tests for the comparator
python3 plugins/scripts/trivy_gate_test.py
```
