---
title: Trunk-based release flow
description: A complete pipeline shape for teams adopting trunk-based development — PR validation with AI review + static analysis, main as release-candidate, manual tagging, automatic stage deploy, manual prod gate with quorum 2.
---

A complete shape — four pipelines, four humans involved
end-to-end — that delivers safety without the parallel-branch overhead
of git-flow. Teams moving from git-flow without giving up safety
adopt these files mostly as-is.

The trick: **merging to main is NOT deploying to production.**
The model below has four mandatory stops between `main` and prod.
Every stop has a clear gate. Nothing is automatic where it
shouldn't be.

## Why this exists

git-flow's `develop` + `release/*` + `hotfix/*` branches give the
illusion of safety: code parks somewhere "not main" until a
release manager decides it's ready. The cost is hidden — every
day of "not on main" is a day of merge conflicts, drift, and
lost context. Trunk-based collapses those into one branch (main)
and moves the safety into the **pipeline shape** instead.

This page is the cookbook *and* the mental-model document. The
four pipelines are the technical answer; the section [Selling
this to skeptics](#selling-this-to-skeptics) is what you bring
to the team meeting.

## The mental model

```
PR opened   →   pr.yaml       (lint + security + tests + build + AI review)
                  ↓ humans approve the PR
push main   →   main.yaml     (CI + main-<sha> image + release-candidate notes)
                  ↓ release manager clicks "Run latest" on release.yaml with TAG=vX.Y.Z
manual      →   release.yaml  (validate + push tag + build + scan + sign + deploy stage + smoke)
                  ↓ release-approvers click Approve on prod.yaml with TAG=vX.Y.Z (quorum 2)
manual      →   prod.yaml     (preflight + deploy prod + smoke + auto-rollback)
```

Four pipelines, one production system. Every step records a run
in the gocdnext audit log; every approval records who clicked.

**Why this recipe ships one pipeline for "cut tag" + "deploy
stage" (and what changed in v0.10.0)**: the release flow is one
manually-triggered pipeline that runs end to end — the release
manager passes `TAG=v1.2.3` at trigger time; the pipeline cuts
the tag, builds the image with that tag, scans + signs +
publishes, and finally deploys to stage. ~15 minutes wall-clock
on a typical project; one human decision ("cut release"); one
button press. Since v0.10.0 gocdnext also supports
`event: [tag]` + `CI_TAG_NAME`, so this CAN be split into a slim
`release.yaml` (cuts the tag) + a `tag.yaml` that auto-fires on
the resulting tag push to do the build/scan/sign/deploy — see
[Variant: split release + tag.yaml](#variant-split-release--tagyaml)
for that shape. Pick whichever fits your team; both are valid.

## Production-readiness prerequisites

The four-pipeline shape below moves safety from branches to
pipelines, but it has THREE hard prerequisites that aren't
optional if you're going to claim "this is production-ready."
Without them, the shape is provenance-for-audit, not enforcement
— a cosign signature nobody verifies, a digest nobody pins, a
rollback that breaks on the first DB migration.

Each prereq is one knob the operator has to set up ONCE on the
cluster (Kyverno install, Helm digest values, migration shape
review). They're cluster-level, not per-pipeline, so the cost
amortises across every project. Walking past them gives you the
provenance for an audit conversation; meeting them gives you the
enforcement that an audit conversation is asking about.

### 1. Cluster verifies signatures at admission

The release pipeline signs the image with cosign. Nothing the
pipeline does makes the signature **load-bearing** unless the
cluster checks it at admission time. Without verification,
signing is a sticker on a glass jar — the lid still opens.

Install **Kyverno** (or sigstore policy-controller, or
Connaisseur — pick one; we use Kyverno here because the
ClusterPolicy DSL is the most operator-readable) and apply a
ClusterPolicy that fails-closed for any image referenced in the
production namespace that lacks a valid cosign signature anchored
to your team's identity:

```yaml title="prod-cluster/kyverno-signature-required.yaml"
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: require-cosign-signature
spec:
  validationFailureAction: Enforce
  webhookConfiguration:
    failurePolicy: Fail            # fail-closed: missing webhook = deny
  rules:
    - name: verify-image-signature
      match:
        any:
          - resources:
              kinds: [Pod]
              namespaces: [prod, prod-system]
      verifyImages:
        - imageReferences:
            - "ghcr.io/my-org/*"
          attestors:
            - entries:
                - keys:
                    publicKeys: |-
                      -----BEGIN PUBLIC KEY-----
                      <your cosign public key>
                      -----END PUBLIC KEY-----
          mutateDigest: true       # rewrite tag→digest so the verified bytes are what actually runs
          required: true
```

Two things this gives you:

- **`mutateDigest: true`** rewrites every `image: …@sha256:abc`
  before the container starts. Even if the deploying pipeline
  used a tag, the cluster runs the digest. That closes the
  mutable-tag-window gap (next prereq).
- **`required: true` + `failurePolicy: Fail`** means a fresh
  pod with NO signature fails admission. Critical for hotfixes
  that bypass the normal release flow — the cluster still
  refuses them.

Verify the policy is live before the first prod deploy goes
through it:

```bash
kubectl run --rm -it --image=ghcr.io/my-org/scratch:no-sig --restart=Never test-deny -n prod
# Error from server: admission webhook "...kyverno..." denied the request: image not signed
```

If that command **succeeds**, the policy isn't enforcing —
re-check `validationFailureAction: Enforce` and the namespace
selector. A trunk-based release flow without this check is a
trunk-based release flow that pretends to verify provenance.

### 2. Deploy by digest, not tag

OCI tags are mutable. Even with signature verification, if the
deploy references `:v1.2.3` and someone repushes that tag
between stage smoke and prod deploy (another release pipeline,
a manual `docker push`, a registry mirror replay), the
production cluster runs a different binary than the one stage
tested + signed. The cosign signature is anchored to the
**digest**, not the tag — verification of the new digest may
even **succeed** (different bytes, but still a signed image
from your registry).

Fix: resolve the tag to a digest the moment the image lands in
the registry, and propagate the digest all the way to prod. The
Helm chart's `image.digest` value (or whatever your chart names
it) is what guarantees "what was tested is what runs."

In `release.yaml`'s build job, after the push:

```yaml
build-and-push:
  stage: build
  uses: ghcr.io/klinux/gocdnext-plugin-buildx@v1
  outputs:
    digest: IMAGE_DIGEST          # `crane digest` against the just-pushed tag
  with:
    image: ghcr.io/my-org/app:${TAG}
    push: "true"
    # The plugin runs `crane digest <image>:<tag>` after push and
    # captures the sha256:… line into IMAGE_DIGEST. Subsequent jobs
    # consume it via outputs.
```

Then `prod.yaml`'s deploy passes `image.digest` (not `image.tag`):

```yaml
deploy-prod:
  stage: deploy
  needs: [preflight]
  secrets: [PROD_KUBECONFIG]
  uses: ghcr.io/klinux/gocdnext-plugin-helm@v1
  with:
    kubeconfig: ${{ PROD_KUBECONFIG }}
    command: |
      upgrade --install my-app charts/my-app
      --namespace prod
      --set image.repository=ghcr.io/my-org/app
      --set image.digest=${{ needs.preflight.outputs.digest }}
      --wait --timeout 10m
```

And the preflight resolves the digest from the upstream release
run's outputs (instead of just verifying the tag exists). The
`check-pipeline-run` plugin returns `outputs.IMAGE_DIGEST` from
the matched release run; if the digest doesn't match what was
just queried with `crane digest …:${TAG}`, that's a mutable-tag
event between release and prod — fail loud.

```yaml
preflight:
  stage: preflight
  needs: [approve-prod]
  secrets: [GOCDNEXT_API_TOKEN]
  uses: ghcr.io/klinux/gocdnext-plugin-check-pipeline-run@v1
  outputs:
    digest: IMAGE_DIGEST
  with:
    api-url: https://gocdnext.example.com
    api-token: ${{ GOCDNEXT_API_TOKEN }}
    project: my-org
    pipeline: release
    tag: ${TAG}
    expected-status: success
    require-digest-match: true    # plugin re-runs `crane digest` and aborts on mismatch
    max-age: 7d
```

After this prereq, "what was tested is what runs" is enforceable
end-to-end. The digest flows through `release.yaml` outputs →
preflight check → Helm value → Kyverno's `mutateDigest` →
container.

### 3. Migrations are forward-only and backward-compatible (expand/contract)

"Rollback is one click" only holds for stateless apps. `helm
rollback` reverts the Helm release; it does NOT revert a database
migration. Trunk-based with frequent deploys requires
expand/contract migrations as a HARD prerequisite — without
them, every prod deploy with a DDL change is a one-way door, and
"rollback" is a lie.

The contract:

- **Forward-only.** No `goose down`, no `flyway undo`. Rollback
  fixes forward via a corrective migration (`fix(db): roll back
  column rename`).
- **Backward-compatible.** A migration deploys BEFORE the code
  that depends on it. Old code (the version we'd rollback to)
  must still work against the new schema.
- **Expand/contract for renames + drops.** Three deploys:
  1. **Expand**: add new column, dual-write from app code, leave
     old column in place. Both schemas work for both code versions.
  2. **Backfill + cut over**: stop reading the old column, drop
     dual-write. Old code still works (reads from the column
     it knows).
  3. **Contract**: drop the old column. Only safe AFTER you're
     confident you won't roll back across the cut-over.

For destructive changes (column drop, table drop, NOT NULL
without default), the expand/contract dance MUST cross at least
ONE release boundary — typically a week — so the rollback target
is always a deploy with the old column still present.

Document this in your repo so it's NOT tribal:

```md title="docs/db-migrations.md"
# Migration contract

- Forward-only. No `goose down`. Use corrective migrations.
- Every migration must work against the PREVIOUS deploy's code.
- Column drops + renames use expand/contract over ≥ 2 releases.
- Destructive DDL inside a deploy = manual gate. Tag the PR
  `migration:destructive`; release approvers verify the
  expand/contract sequence before approving.
```

The fourth pipeline (`prod.yaml`) doesn't enforce this — there's
no automated way to check "is this migration backward-
compatible?". It enforces the gate: a PR labeled
`migration:destructive` triggers a higher approval quorum via
`quorum_by_label` (`destructive: 3` instead of the default 2).
That makes the operator think twice; the contract above does
the rest.

### What follows assumes these three are in place

The remainder of this page (branching, the four pipelines,
hotfix flow, rollback) treats the three prerequisites above as
done. If your cluster doesn't verify signatures, you can still
adopt the four pipelines for everything else they give you (PR
hygiene, stage smoke, audit trail, quorum gates) — but
"production-ready" is a softer claim. Promote it to the actual
claim once the three checks are in place.

## Branching contract

### DO

- Keep PRs small: < 400 LOC ideal, < 800 LOC max.
- Keep branches short: < 2 days of life.
- Conventional Commits in merges: `feat(scope):`, `fix(scope):`,
  `chore:`, `docs:`, `refactor:`, `BREAKING CHANGE:` in the body
  to force major.
- Squash-merge to main (linear history; squash title becomes the
  CHANGELOG entry).
- Hotfixes use the same flow with a `hotfix` label.

### DON'T

- Long-lived branches (`release/*`, `develop`, `staging`).
- Force-push to main.
- Merge without PR approval + green `pr.yaml`.
- Tag from CLI (always via `release.yaml`).
- Skip stages "because it's small".

### GitHub branch protection on `main`

In repo settings → Branches → main:

- ✓ Require pull request before merging
  - ✓ Require approvals: 1
  - ✓ Dismiss stale approvals when new commits push
- ✓ Require status checks to pass — required: `pr / build` (the
  final job of pr.yaml; the check name comes from the gocdnext
  webhook → status API integration)
- ✓ Require linear history
- ✓ Restrict who can push tags (only the gocdnext service
  account that runs `release.yaml`)

### Recommended `CONTRIBUTING.md`

```md
# Contributing

## Workflow

1. Branch off main: `git checkout -b feat/my-thing`
2. Open a PR early (draft is fine).
3. `pr.yaml` runs automatically on every push to the PR —
   lint, security scans, tests, build, AI review.
4. Wait for 1 reviewer + green `pr.yaml`.
5. Squash-merge with a Conventional Commits title:
   - `feat(scope): description` → minor bump
   - `fix(scope): description`  → patch bump
   - `BREAKING CHANGE:` in body → major bump

## Releasing

Release manager opens gocdnext → `release` pipeline → Run latest,
passing `TAG=vX.Y.Z` at trigger time. The pipeline pushes the git
tag, builds the image, scans, signs, and deploys to stage in one
go (~15 min wall-clock).

## Hotfixes

Same flow as features. Add the `hotfix` label on the PR; the
prod approval gate reads this label via `quorum_by_label`
(shipped in v0.13.0) to drop `required:` from 2 to 1.
```

## Pipeline 1: `pr.yaml`

Triggers on PR open / sync. Validates everything that human
review can't catch with eyes alone.

```yaml title=".gocdnext/pr.yaml"
name: pr

when:
  event: [pull_request]

stages: [lint, security, test, build, review]

jobs:
  # ---------- lint (parallel, fail fast) -----------------------
  lint-go:
    stage: lint
    uses: ghcr.io/klinux/gocdnext-plugin-golangci-lint@v1
    cache:
      - key: golangci-{{ hash "go.sum" }}
        paths: [.go-mod, .go-cache, .golangci-cache]
    with:
      args: ./...

  # ---------- security (parallel) ------------------------------
  gitleaks:
    stage: security
    uses: ghcr.io/klinux/gocdnext-plugin-gitleaks@v1
    with:
      scan-mode: dir
      exit-code: "1"
      allowlist-paths: "docs/, test/fixtures/"

  trivy-fs:
    stage: security
    uses: ghcr.io/klinux/gocdnext-plugin-trivy@v1
    cache:
      - key: trivy-db
        paths: [.cache/trivy]
    with:
      scan-type: fs
      target: .
      severity: HIGH,CRITICAL
      exit-code: "1"
      ignore-unfixed: "true"

  sonar-pr:
    stage: security
    secrets: [SONAR_TOKEN]
    uses: ghcr.io/klinux/gocdnext-plugin-sonar@v1
    cache:
      - key: sonar-{{ hash "**/pom.xml" }}
        paths: [.sonar-cache, .m2-repo]
    with:
      host-url: https://sonarcloud.io
      organization: my-org
      project-key: my-org_my-app
      pull-request-key: ${CI_PULL_REQUEST_KEY}
      pull-request-branch: ${CI_PULL_REQUEST_BRANCH}
      pull-request-base: ${CI_PULL_REQUEST_BASE}
      wait-for-quality-gate: "true"
      quality-gate-timeout: "600"

  # ---------- test (after lint + security) ---------------------
  unit:
    stage: test
    needs: [lint-go]
    uses: ghcr.io/klinux/gocdnext-plugin-go@v1
    cache:
      - key: go-{{ hash "go.sum" }}
        paths: [.go-mod, .go-cache]
    with:
      command: test -race ./...
    test_reports: ["**/*-junit.xml"]

  integration:
    stage: test
    needs: [unit]
    docker: true                        # testcontainers
    uses: ghcr.io/klinux/gocdnext-plugin-go@v1
    cache:
      - key: go-{{ hash "go.sum" }}
        paths: [.go-mod, .go-cache]
    with:
      command: test -race -tags=integration ./...
    test_reports: ["**/*-junit.xml"]

  # ---------- build (no push — only validates) -----------------
  build:
    stage: build
    docker: true
    needs: [integration, trivy-fs, sonar-pr, gitleaks]
    uses: ghcr.io/klinux/gocdnext-plugin-buildx@v1
    with:
      image: ghcr.io/my-org/my-app
      tags: "pr-${CI_PULL_REQUEST_KEY}"
      push: "false"                     # PR doesn't publish
      cache: registry

  # ---------- AI review (comments on the PR) -------------------
  ai-review:
    stage: review
    needs: [build]
    secrets: [ANTHROPIC_API_KEY, GITHUB_TOKEN]
    uses: ghcr.io/klinux/gocdnext-plugin-ai-review@v1
    artifacts:
      optional: [.ai-review]
    with:
      provider: claude
      mode: pr-comment
      repo: my-org/my-app
      pr-number: ${CI_PULL_REQUEST_KEY}
      severity-threshold: warning
      # Advisory mode for the first month; flip fail-on-error to
      # "true" once the team has calibrated what severities mean.
      fail-on-error: "false"

notifications:
  - on: failure
    uses: ghcr.io/klinux/gocdnext-plugin-slack@v1
    secrets: [SLACK_WEBHOOK]
    with:
      webhook: ${{ SLACK_WEBHOOK }}
      channel: "#ci-alerts"
      template: |
        :x: PR #${CI_PULL_REQUEST_KEY} ${CI_PIPELINE_ID} failed
        ${CI_COMMIT_SHORT_SHA} on ${CI_PULL_REQUEST_BRANCH}
```

Notes:
- The implicit project material auto-fires on PR when the
  pipeline has `when.event: [pull_request]` at the top level
  (the webhook handler at `server/internal/webhook/pull_request.go`
  checks the material's events list, which is derived from
  `TriggerEvents`).
- `CI_PULL_REQUEST_KEY`, `CI_PULL_REQUEST_BRANCH`,
  `CI_PULL_REQUEST_BASE`, `_TITLE`, `_AUTHOR`, `_URL` are injected
  server-side from the webhook payload (since v0.9.0 — issue #9).
  No operator wiring needed; PR runs see them automatically. Push
  / manual runs skip them silently. See
  [environment variables](/gocdnext/docs/pipelines/yaml-reference/#environment-variables).
- Sonar's `wait-for-quality-gate: "true"` blocks PR merge if the
  gate fails — the strict gate. Pair with `gocdnext/ai-review`'s
  `fail-on-error: "true"` for double gating once you trust both.

## Pipeline 2: `main.yaml`

Triggers on push to main (post-merge). Re-runs the full CI suite
(defense against the rare flaky PR run), publishes a `main-<sha>`
image for preview environments, and generates release-candidate
notes the team sees in Slack so the release manager knows what's
sitting in main vs the last tag.

**Crucial**: `main.yaml` **does NOT** tag. Tagging is a conscious
decision via `release.yaml`. The `main-<sha>` image is for
preview / dev environments — **never prod**.

```yaml title=".gocdnext/main.yaml"
name: main

when:
  event: [push]
  branch: [main]

stages: [test, build, candidate]

jobs:
  test:
    stage: test
    uses: ghcr.io/klinux/gocdnext-plugin-go@v1
    cache:
      - key: go-{{ hash "go.sum" }}
        paths: [.go-mod, .go-cache]
    with:
      command: test -race ./...
    test_reports: ["**/*-junit.xml"]

  build:
    stage: build
    docker: true
    needs: [test]
    secrets: [GHCR_USERNAME, GHCR_TOKEN]
    uses: ghcr.io/klinux/gocdnext-plugin-buildx@v1
    with:
      image: ghcr.io/my-org/my-app
      tags: |
        main-${CI_COMMIT_SHORT_SHA}
        main-latest
      push: "true"
      registry: ghcr.io
      username: ${{ GHCR_USERNAME }}
      password: ${{ GHCR_TOKEN }}

  candidate-notes:
    stage: candidate
    needs: [build]
    uses: ghcr.io/klinux/gocdnext-plugin-release-notes@v1
    with:
      format: conventional
      output: dist/CANDIDATE.md
      heading: "## Candidate since last tag"
    artifacts:
      paths: [dist/CANDIDATE.md]

  notify-candidate:
    stage: candidate
    needs: [candidate-notes]
    needs_artifacts:
      - from_job: candidate-notes
        paths: [dist/CANDIDATE.md]
    secrets: [SLACK_WEBHOOK]
    image: alpine:3.20
    script:
      - apk add --no-cache curl jq
      - |
        NOTES=$(cat dist/CANDIDATE.md)
        PAYLOAD=$(jq -n --arg text "$NOTES" \
          '{text: ":package: Release candidate ready (main is green)",
            attachments: [{text: $text}]}')
        curl -fsSL -X POST -H "Content-Type: application/json" \
          -d "$PAYLOAD" "$SLACK_WEBHOOK"
```

Pro git-flow dev this is the killer: "olha, eu mergei na main e a
imagem `main-<sha>` apareceu — mas ela NÃO foi pra prod. A
release-candidate notes diz o que tem na main desde a última
release; o release manager decide quando cortar."

## Pipeline 3: `release.yaml`

Triggers manually. The release manager passes `TAG=vX.Y.Z` at
trigger time. The pipeline validates → pushes the git tag →
builds the image → scans → signs by digest → deploys to stage →
smoke-tests stage. One pipeline does the full release-candidate
chain end-to-end (~15 min wall-clock on a typical project).

### One pipeline vs. split release + tag (your call since v0.10.0)

This recipe uses **one** pipeline for cut-tag + build/scan/sign/
deploy because it's the simpler shape. As of v0.10.0 gocdnext
ships `event: [tag]` + `CI_TAG_NAME` / `CI_TAG_MESSAGE` /
`CI_TAG_AUTHOR` (the git ref target SHA arrives via the existing
`CI_COMMIT_SHA`), so you CAN split this into a `release.yaml`
that only cuts the tag and a `tag.yaml` that auto-fires the
build/scan/sign/deploy when the tag push lands —
see the [Variant: split release + tag.yaml](#variant-split-release--tagyaml)
section below for the shape. Pick the single-pipeline form if
you want one button push to cut + deploy stage; pick the split
form if you want tag pushes from any source (CLI, GitHub UI,
another automation) to drive the build automatically.

### Why scan-after-publish (not candidate-then-promote)

Multi-arch images can't live in a local Docker daemon — buildx
needs a registry to publish a multi-platform manifest list. And
`gocdnext/docker-push` is a `docker tag` + `docker push` from
the agent's daemon, which **doesn't preserve the multi-arch
manifest** during retag. So the "build candidate → scan → promote"
flow that works for single-arch breaks silently for multi-arch.

The honest pattern for trunk-based + multi-arch + this plugin
set: **publish directly to the final tag, scan after publish, and
treat scan failure as "manually delete the tag"**. The Trivy job
fails the run on HIGH/CRITICAL CVE; the operator's runbook
includes `oras delete ghcr.io/my-org/my-app:${TAG}` (or the
equivalent for your registry) when this happens. Acceptable
trade-off: scan failures should be rare on a properly-maintained
image; the published-but-unscanned window is the time between
build and the trivy job (~30s).

(If you need scan-before-publish AND multi-arch in one pipeline,
add `crane copy` / `skopeo copy` / `buildx imagetools create`
via a custom script step. Out of scope for the recipe here.)

```yaml title=".gocdnext/release.yaml"
name: release

when:
  event: [manual]

# Operator passes at trigger time. Pipeline refuses to run if
# empty or if the tag already exists.
variables:
  TAG: ""

stages: [validate, tag, build, scan, sign, stage]

jobs:
  # ---------- validate inputs + repo state ---------------------
  validate:
    stage: validate
    image: alpine:3.20
    script:
      - apk add --no-cache git
      - |
        if [ -z "$TAG" ]; then
          echo "❌ TAG variable required at trigger time (e.g. v1.2.3)"
          exit 1
        fi
        case "$TAG" in
          v[0-9]*) ;;
          *)
            echo "❌ TAG must start with 'v' and a digit (got: $TAG)"
            exit 1
            ;;
        esac
        if git rev-parse "refs/tags/$TAG" >/dev/null 2>&1; then
          echo "❌ tag $TAG already exists"
          exit 1
        fi
        LAST=$(git describe --tags --abbrev=0 2>/dev/null || echo "v0.0.0")
        COUNT=$(git rev-list "$LAST..HEAD" --count)
        if [ "$COUNT" -eq 0 ]; then
          echo "❌ No commits since $LAST — nothing to release"
          exit 1
        fi
        echo "✓ ready to cut $TAG ($COUNT commits since $LAST)"

  # ---------- generate release notes + push tag ----------------
  release-notes:
    stage: tag
    needs: [validate]
    uses: ghcr.io/klinux/gocdnext-plugin-release-notes@v1
    with:
      format: conventional
      output: dist/RELEASE_NOTES.md
      heading: "## ${TAG}"
    artifacts:
      paths: [dist/RELEASE_NOTES.md]

  create-tag:
    stage: tag
    needs: [release-notes]
    needs_artifacts:
      - from_job: release-notes
        paths: [dist/RELEASE_NOTES.md]
    secrets: [GH_RELEASE_TOKEN]
    image: alpine:3.20
    script:
      - apk add --no-cache git
      - |
        git config user.email "ci-release@my-org.example.com"
        git config user.name "release-bot"
        git tag -a "$TAG" -F dist/RELEASE_NOTES.md

        # GIT_ASKPASS pattern: the token never lands on argv (vs
        # `git remote set-url ...token...`) and never persists in
        # `.git/config` (vs embedding in the remote URL). The
        # helper is a tiny shell script that prints credentials
        # from inherited env; the trap wipes it on exit.
        ASKPASS=$(mktemp)
        chmod 700 "$ASKPASS"
        trap 'rm -f "$ASKPASS"' EXIT
        cat > "$ASKPASS" <<'EOF'
        #!/bin/sh
        case "$1" in
            Username*) echo "x-access-token" ;;
            Password*) echo "$GH_RELEASE_TOKEN" ;;
        esac
        EOF
        chmod +x "$ASKPASS"

        # Assumes `origin` is the https://github.com/my-org/my-app.git
        # URL the agent cloned (no embedded creds). If your agent
        # uses SSH, replace with an ssh-key-based push instead.
        GIT_ASKPASS="$ASKPASS" git push origin "$TAG"
        echo "==> Pushed tag $TAG"

  # ---------- build immutable artefact -------------------------
  build:
    stage: build
    needs: [create-tag]
    docker: true
    secrets: [GHCR_USERNAME, GHCR_TOKEN]
    uses: ghcr.io/klinux/gocdnext-plugin-buildx@v1
    # `digest:` output is the sha256:… of the built+pushed manifest;
    # downstream jobs (scan, sign, prod's preflight) consume this so
    # the operation runs against an immutable digest even if someone
    # races a `docker push` against the tag.
    # See [Prerequisite 2: Deploy by digest, not tag](#2-deploy-by-digest-not-tag).
    outputs:
      digest: IMAGE_DIGEST
    with:
      image: ghcr.io/my-org/my-app
      # NOTE: `latest` is deliberately NOT in this list. The first
      # cut of this doc pushed `latest` on every release; it
      # contradicts the immutability discourse (operators end up
      # pulling a moving target) and is exactly the kind of tag
      # that Kyverno's `mutateDigest` is forced to clean up. Drop
      # it and reference tags by `${TAG}` or by digest.
      tags: ${TAG}
      platforms: linux/amd64,linux/arm64
      push: "true"
      registry: ghcr.io
      username: ${{ GHCR_USERNAME }}
      password: ${{ GHCR_TOKEN }}
      cache: registry

  # ---------- scan published image -----------------------------
  trivy-image:
    stage: scan
    docker: true
    needs: [build]
    cache:
      - key: trivy-db
        paths: [.cache/trivy]
    uses: ghcr.io/klinux/gocdnext-plugin-trivy@v1
    with:
      scan-type: image
      # Scan the DIGEST emitted by build, not the tag — defends
      # against the (unlikely but real) race where someone pushes
      # over the tag between build and scan. The bytes that get
      # signed below are the SAME bytes scanned here.
      target: ghcr.io/my-org/my-app@${{ needs.build.outputs.digest }}
      severity: HIGH,CRITICAL
      exit-code: "1"          # run-fail = operator deletes the tag

  # ---------- sign via plugin's key-content input --------------
  # The gocdnext/cosign plugin accepts `key-content:` which pipes
  # the private-key bytes straight from `secrets:` — the plugin's
  # entrypoint writes them to a 0600 mktemp file and a `trap`
  # wipes it on exit. The key never persists in the artifact
  # backend (S3/GCS/filesystem) and (since v0.7.x) never lands on
  # the docker run argv either — the agent's Docker engine
  # propagates env values via cmd.Env + `docker run -e KEY`
  # (name-only on argv) so secret-bearing values are invisible
  # to `ps auxww` on the host.
  #
  # No `docker: true` — cosign signs via the registry API; it
  # doesn't need the host docker socket. Keeping it off reduces
  # the blast radius of the job that holds the private key.
  #
  # Registry creds are required: `cosign sign` uploads the
  # signature manifest, which on a private registry (like
  # private GHCR) needs auth.
  cosign-sign:
    stage: sign
    needs: [trivy-image]
    secrets: [COSIGN_PRIVATE_KEY, COSIGN_PASSWORD, GHCR_USERNAME, GHCR_TOKEN]
    uses: ghcr.io/klinux/gocdnext-plugin-cosign@v1
    with:
      # Pin by digest, NOT tag. Cosign anchors the signature to
      # the manifest digest regardless of how you invoke it, but
      # passing the digest explicitly closes the
      # tag-resolved-twice race: build pushed digest A under tag,
      # someone races a push of digest B, scan ran on A, cosign
      # would sign B if it re-resolves the tag here. Reusing
      # `needs.build.outputs.digest` ensures sign sees the same
      # bytes that scan blessed.
      image: ghcr.io/my-org/my-app@${{ needs.build.outputs.digest }}
      action: sign
      key-content: ${{ COSIGN_PRIVATE_KEY }}
      key-password: ${{ COSIGN_PASSWORD }}
      registry: ghcr.io
      username: ${{ GHCR_USERNAME }}
      password: ${{ GHCR_TOKEN }}

  github-release:
    stage: sign
    needs: [cosign-sign]
    secrets: [GH_RELEASE_TOKEN]
    uses: ghcr.io/klinux/gocdnext-plugin-github-release@v1
    with:
      tag: ${TAG}
      title: "Release ${TAG}"
      token: ${{ GH_RELEASE_TOKEN }}
      generate-notes: "true"

  # ---------- auto-deploy to stage -----------------------------
  deploy-stage:
    stage: stage
    needs: [cosign-sign]
    secrets: [STAGE_KUBECONFIG]
    uses: ghcr.io/klinux/gocdnext-plugin-helm@v1
    with:
      # helm plugin auto-detects kubeconfig as inline YAML,
      # base64-encoded YAML, or workspace-relative path.
      kubeconfig: ${{ STAGE_KUBECONFIG }}
      command: |
        upgrade --install my-app charts/my-app
        --namespace stage
        --set image.tag=${TAG}
        --wait --timeout 5m

  smoke-stage:
    stage: stage
    needs: [deploy-stage]
    image: alpine:3.20
    script:
      - apk add --no-cache curl
      - |
        for url in $(cat ./tests/smoke-urls.txt); do
          if ! curl -fsSL "$url"; then
            echo "❌ smoke failed on $url"
            exit 1
          fi
        done
        echo "✓ stage smoke passed for ${TAG}"

notifications:
  - on: success
    uses: ghcr.io/klinux/gocdnext-plugin-slack@v1
    secrets: [SLACK_WEBHOOK]
    with:
      webhook: ${{ SLACK_WEBHOOK }}
      channel: "#deploys"
      template: |
        :white_check_mark: ${TAG} deployed to STAGE.
        Run prod deploy manually when ready.
```

Important: prod is **not touched**. The Slack message says
explicitly "Run prod deploy manually when ready". That's the
contract: published image = release candidate, not production
truth.

### Keyless cosign (alternative when OIDC is available)

The recipe above uses a key-based sign because most teams have a
PEM key + passphrase but not Sigstore OIDC. When your platform
DOES have OIDC (Fulcio + Rekor on the public network, or a
private Sigstore deployment), prefer keyless — no key to
manage, no rotation. Drop the secrets + key inputs:

```yaml
cosign-sign:
  stage: sign
  needs: [trivy-image]
  secrets: [GHCR_USERNAME, GHCR_TOKEN]
  uses: ghcr.io/klinux/gocdnext-plugin-cosign@v1
  with:
    image: ghcr.io/my-org/my-app:${TAG}
    action: sign
    # No key or key-content → plugin signs keyless via Fulcio.
    registry: ghcr.io
    username: ${{ GHCR_USERNAME }}
    password: ${{ GHCR_TOKEN }}
```

This requires a workload-identity setup gocdnext doesn't ship
today (the agent's pod needs a projected SA token + the
registry needs to accept Fulcio certificates). Use it if your
platform team has this; otherwise the key-content pattern above
is the realistic path.

### Cleaner downstream chains with native job outputs (v0.11+)

Since v0.11.0 gocdnext supports
[structured job outputs](/gocdnext/docs/pipelines/yaml-reference/#job-outputs-outputs).
The recipes above use the legacy "downstream is `image:` + `script:` +
`source .gocdnext/foo.env`" pattern because that's what worked on
older agents; for new pipelines, declare `outputs:` on the producer
and reference `${{ needs.X.outputs.Y }}` directly in any consumer's
`with:` or `env:`. That lets the cosign-sign step (and every other
step that needs a runtime value from a prior job) stay as a clean
`uses:` plugin invocation instead of inlining shell.

`gocdnext/semver-bump@v1` and `gocdnext/image-copy@v1` already
write both the legacy workspace file AND `$GOCDNEXT_OUTPUT_FILE`,
so the migration is a one-line declaration on the producer + a
one-token substitution on the consumer.

### Variant: split release + tag.yaml

Since v0.10.0 gocdnext routes `event: [tag]` webhooks and emits
`CI_TAG_NAME` / `CI_TAG_MESSAGE` / `CI_TAG_AUTHOR` (the git ref
target SHA is exposed via the existing `CI_COMMIT_SHA` — that's
the git SHA the tag points at, NOT an OCI image digest, so use
`CI_TAG_NAME` for image references the registry will resolve).
This lets `release.yaml` do **just** the tag cut, and a separate
`tag.yaml` auto-fires on tag push to do the build/scan/sign/
deploy. The split is cleaner because the build pipeline doesn't
need a manual TAG variable — it reads `${CI_TAG_NAME}` from the
webhook payload — and any tag pushed via CLI or GitHub UI fires
the same build, not just tags from the Run-latest dashboard.

```yaml title=".gocdnext/release.yaml (slim)"
name: release
when:
  event: [manual]

variables:
  TAG: ""

stages: [validate, tag]

jobs:
  validate:
    stage: validate
    image: alpine:3.20
    script: [apk add --no-cache git, |
      # Same validate body as the single-pipeline variant above.
      ...]

  create-tag:
    stage: tag
    needs: [validate]
    secrets: [GH_RELEASE_TOKEN]
    image: alpine:3.20
    script: [apk add --no-cache git, |
      # Same GIT_ASKPASS push as the single-pipeline variant.
      # The tag push fires the GitHub webhook → tag.yaml takes over.
      ...]
```

```yaml title=".gocdnext/tag.yaml"
name: tag
when:
  event: [tag]

stages: [build, scan, sign, stage]

jobs:
  build:
    stage: build
    docker: true
    secrets: [GHCR_USERNAME, GHCR_TOKEN]
    uses: ghcr.io/klinux/gocdnext-plugin-buildx@v1
    with:
      image: ghcr.io/my-org/my-app
      tags: |
        ${CI_TAG_NAME}
        latest
      platforms: linux/amd64,linux/arm64
      push: "true"
      registry: ghcr.io
      username: ${{ GHCR_USERNAME }}
      password: ${{ GHCR_TOKEN }}
      cache: registry

  trivy-image:
    stage: scan
    docker: true
    needs: [build]
    uses: ghcr.io/klinux/gocdnext-plugin-trivy@v1
    with:
      scan-type: image
      target: ghcr.io/my-org/my-app:${CI_TAG_NAME}
      severity: HIGH,CRITICAL
      exit-code: "1"

  cosign-sign:
    stage: sign
    needs: [trivy-image]
    secrets: [COSIGN_PRIVATE_KEY, COSIGN_PASSWORD, GHCR_USERNAME, GHCR_TOKEN]
    uses: ghcr.io/klinux/gocdnext-plugin-cosign@v1
    with:
      # cosign resolves the tag to its manifest digest at sign time
      # and anchors the signature to that digest — passing the
      # mutable tag here is correct, the signature itself is
      # immutable. Same pattern as the single-pipeline release.yaml.
      image: ghcr.io/my-org/my-app:${CI_TAG_NAME}
      action: sign
      key-content: ${{ COSIGN_PRIVATE_KEY }}
      key-password: ${{ COSIGN_PASSWORD }}
      registry: ghcr.io
      username: ${{ GHCR_USERNAME }}
      password: ${{ GHCR_TOKEN }}

  deploy-stage:
    stage: stage
    needs: [cosign-sign]
    secrets: [STAGE_KUBECONFIG]
    uses: ghcr.io/klinux/gocdnext-plugin-helm@v1
    with:
      kubeconfig: ${{ STAGE_KUBECONFIG }}
      command: |
        upgrade --install my-app charts/my-app
        --namespace stage
        --set image.tag=${CI_TAG_NAME}
        --wait --timeout 5m
```

**How it routes**: tag pushes match by URL (a tag points at a SHA
that may not be on any branch, so branch-based routing can't
work). Every git material on this repo with `events: [tag]`
fires; materials without `tag` in their list are silently
skipped. The implicit project material auto-inherits its events
from the pipeline-level `when.event:`, so the `tag.yaml` above
needs no `materials:` block.

`prod.yaml` is unchanged — it still triggers manually with
`TAG=vX.Y.Z` passed at trigger time, since prod promotion is a
human gate by design (not a webhook follow-on).

## Pipeline 4: `prod.yaml`

Triggers manually. The only pipeline that can touch production.
Approval block with `approver_groups: [release-approvers]` and
`required: 2` ensures no single actor can ship to prod alone.

```yaml title=".gocdnext/prod.yaml"
name: prod

when:
  event: [manual]

variables:
  # Release-approver picks the tag at trigger time.
  TAG: v0.0.0

stages: [gate, preflight, deploy, verify]

jobs:
  approve-prod:
    stage: gate
    approval:
      description: |
        Promote ${TAG} to production?
        Check that stage smoke results are green for this tag.
      approver_groups: [release-approvers]
      required: 2                       # quorum 2 — default for normal promotions
      quorum_by_label:                  # v0.13.0+ — PR-label-driven override
        hotfix: 1                       # `hotfix` label on the originating PR → quorum 1
        # add `breaking-change: 3` if your team prefers stricter for risky tags

  preflight:
    stage: preflight
    needs: [approve-prod]
    # Since v0.12.0 this is a typed plugin call instead of inline
    # curl + jq. Confirms a terminal-success run of `release`
    # exists for ${TAG} via the gocdnext REST API AND that the
    # resolved digest still matches what release.yaml signed
    # (`require-digest-match: true` since v0.14.x — re-runs
    # `crane digest` against the current registry state and
    # aborts if the tag has been re-pushed since release ran).
    # Fails the gate loud (exit 1) when either check fails — the
    # prod deploy chain stays red. The digest flows out as an
    # output for the deploy job below.
    secrets: [GOCDNEXT_API_TOKEN]
    uses: ghcr.io/klinux/gocdnext-plugin-check-pipeline-run@v1
    outputs:
      digest: IMAGE_DIGEST
      run_url: RUN_URL
    with:
      api-url: https://gocdnext.example.com
      api-token: ${{ GOCDNEXT_API_TOKEN }}
      project: my-org
      pipeline: release
      tag: ${TAG}
      expected-status: success
      require-digest-match: true
      max-age: 7d
    artifacts:
      paths: [".gocdnext/preflight.env"]

  deploy-prod:
    stage: deploy
    needs: [preflight]
    secrets: [PROD_KUBECONFIG]
    uses: ghcr.io/klinux/gocdnext-plugin-helm@v1
    with:
      kubeconfig: ${{ PROD_KUBECONFIG }}
      # Pin by DIGEST (not tag) so the bytes the cluster runs are
      # exactly the bytes release.yaml signed. See
      # [Prerequisite 2: Deploy by digest, not tag](#2-deploy-by-digest-not-tag)
      # for why this isn't optional. Kyverno's `mutateDigest`
      # would catch a tag-only deploy too, but pinning here gives
      # us a deterministic Helm release shape (revision N always
      # references the same bytes) which makes auto-rollback
      # behave predictably.
      command: |
        upgrade --install my-app charts/my-app
        --namespace prod
        --set image.repository=ghcr.io/my-org/app
        --set image.digest=${{ needs.preflight.outputs.digest }}
        --wait --timeout 10m

  smoke-prod:
    stage: verify
    needs: [deploy-prod]
    secrets: [PROD_KUBECONFIG, SLACK_WEBHOOK]
    image: alpine:3.20
    script:
      # apk's helm package (community repo on Alpine 3.19+) gives
      # us `helm rollback` from a tiny image without bundling a
      # custom one. Same as bash/curl/openssl pattern.
      - apk add --no-cache bash curl jq helm openssl
      - |
        # Write kubeconfig to a private file the rollback can use.
        # Inline secret value goes through stdin → file, never argv.
        export KUBECONFIG=$(mktemp)
        chmod 600 "$KUBECONFIG"
        printf '%s' "$PROD_KUBECONFIG" > "$KUBECONFIG"
        trap 'rm -f "$KUBECONFIG"' EXIT

        if ! ./tests/smoke-prod.sh; then
          echo "❌ prod smoke failed — auto-rollback"
          # `helm rollback <release> 0` rolls back to the prior
          # revision (0 = "one before current"). More reliable
          # than trying to compute "previous tag" under
          # concurrent partial deploys.
          helm rollback my-app 0 -n prod --wait --timeout 10m
          curl -fsSL -X POST "$SLACK_WEBHOOK" \
            -H "Content-Type: application/json" \
            -d "{\"text\":\":fire: prod rollback (smoke failed on ${TAG})\"}"
          exit 1
        fi
        echo "✓ prod smoke passed for ${TAG}"

notifications:
  - on: success
    uses: ghcr.io/klinux/gocdnext-plugin-slack@v1
    secrets: [SLACK_WEBHOOK]
    with:
      webhook: ${{ SLACK_WEBHOOK }}
      channel: "#deploys"
      template: ":rocket: ${TAG} live in PROD"
  - on: failure
    uses: ghcr.io/klinux/gocdnext-plugin-slack@v1
    secrets: [SLACK_WEBHOOK]
    with:
      webhook: ${{ SLACK_WEBHOOK }}
      channel: "#oncall"
      template: |
        :fire: PROD deploy ${TAG} failed — auto-rollback may have triggered.
        Investigate: ${CI_RUN_ID}
```

Why the auto-rollback is non-negotiable for trunk-based: it
removes the "what if it breaks?" fear. The previous Helm
revision is always one `helm rollback` away.

## Hotfix flow

Same four files, two differences:

1. PR has the `hotfix` label. The reviewer policy can be
   relaxed (e.g. branch protection rule "Allow specific actors
   to bypass" for the on-call user).
2. `prod.yaml`'s approval gate reads the `hotfix` label natively
   via `quorum_by_label` (shipped v0.13.0). The override is a
   snapshot at run materialisation — the PR's labels are read
   once when the run is created, no pre-step or GitHub-API
   round-trip. See [Approval gates](/gocdnext/docs/concepts/approvals/#pr-label-driven-quorum).

The path through the pipelines is **identical**: PR → main →
tag → stage → prod. Hotfix is **faster**, not **safer**. Code
review (human + AI), security scans, build immutable, stage
smoke don't disappear.

## Rollback

Two mechanisms, ONE canonical path, an explicit escape valve.
The four-pipeline shape gives you both `helm rollback` (Helm
revision-aware, used by `prod.yaml`'s auto-rollback on smoke
failure) and "re-trigger `prod.yaml` with the previous tag"
(human-driven, full pipeline pass). Pre-v0.14 versions of this
doc described both without picking one — operators ended up
mixing them, leaving the cluster in different states (Helm
revision points at chart values from N-1, re-deploy uses values
at HEAD with image of N-1, drift). Pick one as the default; use
the other only when the default doesn't apply.

### When rollback is safe (default path)

A rollback is safe when **all** of these hold:

1. The migration contract from
   [Prerequisite 3](#3-migrations-are-forward-only-and-backward-compatible-expandcontract)
   is followed: every migration between current and target is
   backward-compatible, no destructive DDL crossed the rollback
   boundary.
2. The signature verification + digest pinning prereqs are in
   place — Kyverno's `mutateDigest` ensures the rolled-back
   container is the bytes that were signed for the target tag,
   not a re-tagged drift.
3. The target tag is one that ran green through `release.yaml`
   AND its `prod.yaml` deploy in the past (i.e., it's a tag
   that has already been to prod successfully). Tags that only
   passed stage haven't proven themselves under prod load.

When all three hold, rollback is one of two flavours below.
When they don't, jump to [Fix-forward](#when-rollback-is-not-safe-fix-forward).

### Auto-rollback (Helm-revision based)

When `smoke-prod` fails immediately after a deploy, the same job
calls `helm rollback my-app 0 -n prod` — Helm steps back to the
prior revision. This is what `prod.yaml` does today; nothing
human-driven is needed. The cluster state matches the previous
revision EXACTLY (same chart values, same image, same secrets)
because Helm tracks the revision atomically.

Use this for: smoke-test failures, immediate post-deploy
regressions caught by automation. Window: the first ~5 minutes
after the deploy lands.

### Manual rollback (re-trigger `prod.yaml` with the previous tag)

After the smoke-test window closes (auto-rollback won't fire),
or when the regression was caught by humans hours/days later,
re-trigger `prod.yaml` with the previous good tag:

```
Dashboard → prod pipeline → Run latest
  TAG: v0.6.4    (the previous known-good)
  → approval gate (quorum 2 — same as forward)
  → preflight (resolves digest from release.yaml)
  → helm upgrade --set image.digest=<v0.6.4 digest>
  → smoke
```

Yes, "shit's broken right now" deploys still need 2 approvers.
Intentional: panic deploys are how incidents compound. The
on-call playbook should include a list of known-good tags so
the approval is < 60 seconds.

Use this for: regressions caught past the smoke window, when
auto-rollback didn't fire, or when chart values changed between
the deploys (Helm revision rollback would also revert the
values change, which may not be what you want).

### When rollback is NOT safe — fix-forward

If the deploy you'd roll back across includes any of these,
**don't roll back** — fix forward:

- A migration that's not backward-compatible (column drop without
  expand/contract, NOT NULL on existing data without default,
  destructive DDL marked by the PR's `migration:destructive`
  label that bypassed the higher-quorum gate).
- A secret rotation that depended on the new code reading the
  rotated value (rolling back means old code with new secret =
  auth failures).
- A feature-flag default change that the previous code can't
  parse (also expand/contract territory; if you cross-deploy
  flag updates, leave the old default working).

Fix-forward = open a hotfix PR with the corrective change, ride
it through `pr.yaml` → `main.yaml` → `release.yaml` →
`prod.yaml` like any other deploy. The hotfix label loosens
quorum on prod from 2 → 1 (per `quorum_by_label`); everything
else stays the same. ~30 minutes wall-clock for the round-trip.

The on-call playbook should call this out explicitly: **the
default is rollback; the exception is fix-forward; the question
that decides is "does the migration / secret / flag survive a
backward jump?"** That single question is the most important
rollback skill an on-call rotation can build.

## Migration plan

For a team currently on git-flow, adopt incrementally so each
phase proves safety before the next lands:

### Week 1-2: `pr.yaml` only

- Land the file, hook on PR open/sync.
- Team continues developing on `develop` if they want.
- Devs see lint/security/test/AI-review results on every PR.
- They start trusting the CI.

### Week 3-4: `main.yaml`

- Land the main pipeline.
- Switch default branch to main; mark `develop` deprecated.
- Devs see main stays green; merges don't break anything
  (prod isn't connected yet).
- Release-candidate notes start showing up in Slack.

### Week 5: `release.yaml`

- Move release management from manual `git tag` + manual build
  scripts to running the release pipeline. Operator passes
  `TAG=vX.Y.Z` at trigger time; the pipeline does git tag +
  build + scan + sign + stage deploy + smoke in one go.
- Team sees the value of stage smoke before prod.

### Week 6+: `prod.yaml`

- Add the prod pipeline with quorum 2.
- The "release-manager-only-can-deploy" flow goes away.
- Delete `develop` branch.
- Update CONTRIBUTING.md to point at this page.

The point of phasing is **psychological**, not technical. Devs
who were scared of trunk-based see each step prove safety. After
phase 5 they're asking "can we ship more often?".

## Selling this to skeptics

If you have devs who think git-flow is the safe option, the
talking points that work:

1. **"Merging to main is NOT deploying to prod."** Show the
   4-stop chain. The mental model fits on one slide.
2. **"Two humans approve every prod deploy."** That's already
   more than your git-flow has — most git-flow shops have
   "release manager + tags" with no second-approval contract.
3. **"Rollback is one click."** Re-run prod.yaml with previous
   tag. Quicker than any merge-conflict-prone git-flow rollback.
4. **"Branches > 800 LOC have 3-5× more bugs in production."**
   Short PRs are the actual safety net — not long-lived
   release branches.
5. **"AI + Sonar + Trivy + Gitleaks + tests + 1 human review
   every PR."** They have more eyes on each change than
   git-flow normally provides.
6. **"Hotfix is the same path, faster."** Show that no shortcut
   skips review.

## Limitations + roadmap

The model above is **mostly** ready to apply against gocdnext as
shipped. The deliberate gaps:

- **PR vars**: ✅ shipped in v0.9.0 ([issue #9](https://github.com/klinux/gocdnext/issues/9)).
  PR runs see `CI_CAUSE=pull_request` plus
  `CI_PULL_REQUEST_KEY` / `_BRANCH` / `_BASE` / `_TITLE` /
  `_AUTHOR` / `_URL` materialised server-side from the webhook
  payload. The `pr.yaml` recipe above uses them directly — no
  `variables:` workaround.
- **Tag-push event + tag env vars**: ✅ shipped in v0.10.0. Tag
  pushes now route via `event: [tag]` (URL-only matching since
  tags don't carry a base branch); the scheduler injects
  `CI_TAG_NAME` / `CI_TAG_MESSAGE` / `CI_TAG_AUTHOR` for any run
  with `cause == "tag"`. The git ref target SHA is on the
  existing `CI_COMMIT_SHA` — same value, no duplicate var. The
  [Variant: split release + tag.yaml](#variant-split-release--tagyaml)
  section above shows the cleaner shape this enables. The
  single-pipeline form remains in the recipe for teams that
  prefer one button push.
- **Multi-arch scan-before-publish**: ✅ shipped in v0.10.0. The
  `gocdnext/image-copy@v1` plugin promotes multi-arch images
  between registries via crane / skopeo / buildx-imagetools
  (operator picks the backend), preserving the manifest list
  end-to-end. Rewrite to scan-before-publish: build to a
  staging registry → trivy-scan staging → image-copy from
  staging to prod registry → cosign-sign by promoted digest.
  The recipe in this page still uses scan-after-publish as the
  single-pipeline shape for simplicity; the
  [Variant: split release + tag.yaml](#variant-split-release--tagyaml)
  section above can be extended to use `image-copy` when teams
  want the cleaner staging-then-promote shape — see the
  plugin's "digest-pinned promotion + cosign sign-by-digest"
  example in the catalog.
- **Per-tag preflight via API**: ✅ shipped in v0.12.0. The
  `gocdnext/check-pipeline-run@v1` plugin queries the gocdnext
  REST API and confirms a target pipeline produced a
  terminal-success run matching the operator's filter
  (`tag:`, `revision:`). Replaces the inline curl + jq in
  `prod.yaml` with a typed plugin + structured outputs (run id,
  URL, revision, counter, finished_at). Supports a poll mode
  for prod chains queued immediately after the release pipeline.
- **Semver bump as plugin**: ✅ shipped in v0.10.0. The
  `gocdnext/semver-bump@v1` plugin auto-computes the next tag
  from Conventional Commits since the prior tag (major on
  `feat!:` / `BREAKING CHANGE:`, minor on `feat:`, patch
  otherwise). Writes a shell-sourceable `.gocdnext/semver.env`
  that downstream `create-tag` jobs source. Combined with
  `event: [tag]` + `CI_TAG_NAME` above, the release flow is now
  "click Run on release.yaml → semver-bump → create-tag → push;
  tag webhook auto-fires tag.yaml" with no operator-typed TAG
  variable anywhere.
- **Cosign by content key**: ✅ shipped in this release. The
  `gocdnext/cosign@v1` plugin now accepts `key-content:` which
  writes the bytes to a 0600 mktemp inside the plugin's
  container and `trap`-wipes on exit. The recipe uses this; no
  artifact persistence, no shell hack with the official cosign
  image (which is distroless + has no shell).
- **PR-label-driven approval quorum**: ✅ shipped in v0.13.0
  via `quorum_by_label` on the `approval:` block. Reads PR
  labels from the run's `cause_detail.pr_labels` (snapshot at
  materialisation) and applies the largest matching override.
  GitHub-only at v0.13.0 ([#11](https://github.com/klinux/gocdnext/issues/11)
  / [#12](https://github.com/klinux/gocdnext/issues/12) for
  GitLab MR / Bitbucket PR).
- **Coverage tab + HTML report preview**: see [issue #8](https://github.com/klinux/gocdnext/issues/8).
  Today coverage = `artifacts.optional:` + Sonar Quality Gate
  covers the gate.

## See also

- [gocdnext/ai-review plugin](/gocdnext/docs/reference/plugins/#ai-review) — Claude + OpenAI code review.
- [gocdnext/sonar plugin](/gocdnext/docs/reference/plugins/#sonar) — SonarQube + SonarCloud Quality Gate.
- [Approval gates](/gocdnext/docs/concepts/approvals/) — `approver_groups` + `required` (quorum).
- [Services lifecycle](/gocdnext/docs/concepts/services/) — sibling service containers (postgres, redis, …) for integration tests.
- [Materials](/gocdnext/docs/concepts/materials/) — implicit project material + `when.event:` triggers.
