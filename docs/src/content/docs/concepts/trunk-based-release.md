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

**Why one pipeline does both "cut tag" and "deploy stage" (not
two)**: gocdnext doesn't yet expose tag-push events as
`event: [tag]` with a `CI_TAG` env var — see
[Limitations](#limitations--roadmap). Until it does, the
release flow is one manually-triggered pipeline that runs end
to end. The release manager passes `TAG=v1.2.3` at trigger
time; the pipeline cuts the tag, builds the image with that tag,
scans + signs + publishes, and finally deploys to stage. ~15
minutes wall-clock on a typical project; one human decision
("cut release"); one button press.

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

Same flow as features. Add the `hotfix` label on the PR; this
flag is read by `prod.yaml`'s approval gate to optionally reduce
the quorum from 2 to 1 (see the team's policy).
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

### Why one pipeline instead of two (release + tag)

Originally this guide described a separate `tag.yaml` triggered
on tag push. That doesn't work today: gocdnext doesn't surface
tag-push events as `event: [tag]` with a `CI_TAG` env var (the
backend stores tag info but the agent doesn't see it as a CI
variable). Until that gap closes (see
[Limitations](#limitations--roadmap)), the safer pattern is one
manually-triggered pipeline that knows the tag because the
operator passes it at trigger time.

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
    with:
      image: ghcr.io/my-org/my-app
      tags: |
        ${TAG}
        latest
      platforms: linux/amd64,linux/arm64
      push: "true"          # scan-after-publish trade-off (see above)
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
      target: ghcr.io/my-org/my-app:${TAG}
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
      # cosign anchors the signature to the manifest DIGEST even
      # when invoked against a tag — the tag is resolved once at
      # sign time and the signature attaches to whatever digest
      # it pointed at within this run.
      image: ghcr.io/my-org/my-app:${TAG}
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
      required: 2                       # quorum 2 — non-negotiable

  preflight:
    stage: preflight
    needs: [approve-prod]
    image: alpine:3.20
    script:
      - apk add --no-cache git
      - |
        # gocdnext's API doesn't expose a queryable "find runs
        # where variable.TAG=X" today — TAG is a pipeline
        # variable, not a column on `runs`. So this preflight
        # does what CAN be verified automatically:
        #   1. The git tag actually exists.
        #   2. (Optional) the image's cosign signature verifies.
        #
        # The substantive verification — "stage smoke passed for
        # this TAG" — happens MANUALLY at approve time. The
        # approvers above are responsible for checking the
        # release pipeline's stage-smoke status in the gocdnext
        # dashboard BEFORE clicking Approve. The pre-flight is
        # the last sanity check, not the primary gate.
        if ! git rev-parse "refs/tags/$TAG" >/dev/null 2>&1; then
          echo "❌ tag $TAG does not exist in this clone"
          echo "   the agent's git fetch may not have pulled it — check materials."
          exit 1
        fi
        echo "✓ git tag $TAG exists"
        # Uncomment when you have cosign.pub committed at the repo root:
        # apk add --no-cache cosign
        # cosign verify --key cosign.pub ghcr.io/my-org/my-app:$TAG
        # echo "✓ cosign signature verified for $TAG"

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
        --set image.tag=${TAG}
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
2. `prod.yaml`'s approval gate can be conditioned by reading the
   tag's associated PR labels via a pre-step (not shown — write
   a small script that queries GitHub's API) to drop `required:
   2` to 1 when `hotfix` is present.

The path through the pipelines is **identical**: PR → main →
tag → stage → prod. Hotfix is **faster**, not **safer**. Code
review (human + AI), security scans, build immutable, stage
smoke don't disappear.

## Rollback

Tags are immutable. Rollback = re-trigger `prod.yaml` with the
previous tag:

```
Dashboard → prod pipeline → Run latest
  TAG: v0.6.4    (the working previous)
  → approval gate (quorum 2)
  → preflight
  → helm upgrade --set image.tag=v0.6.4
  → smoke
```

The same approval gate the forward deploy used governs the
rollback. Yes, this means "shit's broken right now" deploys still
need 2 approvers. That's intentional: panic deploys are how
incidents compound. The on-call playbook should include a list
of known-good tags so the approval is < 60 seconds.

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
- **Tag-push event + `CI_TAG` not surfaced**: the model above
  collapses release.yaml + tag.yaml into one manually-triggered
  pipeline because gocdnext doesn't propagate tag-push events
  to the agent's env as a usable `CI_TAG`. Once it does, the
  recipe can split into release.yaml (cuts the tag) +
  tag.yaml (auto-fires on tag push, builds + deploys stage),
  which is the cleaner shape. Until then, one manual pipeline
  is the honest answer.
- **Multi-arch scan-before-publish**: `gocdnext/docker-push`'s
  `docker tag` + `docker push` flow doesn't preserve multi-arch
  manifest lists during retag. The recipe uses scan-after-
  publish + delete-on-fail. Roadmap: a registry-side image-copy
  plugin (wrapping `crane copy` / `skopeo copy` /
  `buildx imagetools create`).
- **Per-tag preflight via API**: `prod.yaml`'s preflight needs a
  small curl + jq script against gocdnext's REST API to
  confirm the tag passed stage. Roadmap: a dedicated plugin or
  built-in input.
- **Semver bump as plugin**: the recipe inlines `git tag` logic
  in a shell script + relies on the operator passing TAG.
  Roadmap: `gocdnext/semver-bump@v1` plugin that auto-computes
  the next tag from conventional commits since the last tag.
- **Cosign by content key**: ✅ shipped in this release. The
  `gocdnext/cosign@v1` plugin now accepts `key-content:` which
  writes the bytes to a 0600 mktemp inside the plugin's
  container and `trap`-wipes on exit. The recipe uses this; no
  artifact persistence, no shell hack with the official cosign
  image (which is distroless + has no shell).
- **PR-label-driven approval quorum**: hotfix-driven quorum-1
  approval needs a small pre-step that queries GitHub for the
  tag's PR labels. Could be a plugin in the future.
- **Coverage tab + HTML report preview**: see [issue #8](https://github.com/klinux/gocdnext/issues/8).
  Today coverage = `artifacts.optional:` + Sonar Quality Gate
  covers the gate.

## See also

- [gocdnext/ai-review plugin](/gocdnext/docs/reference/plugins/#ai-review) — Claude + OpenAI code review.
- [gocdnext/sonar plugin](/gocdnext/docs/reference/plugins/#sonar) — SonarQube + SonarCloud Quality Gate.
- [Approval gates](/gocdnext/docs/concepts/approvals/) — `approver_groups` + `required` (quorum).
- [Services lifecycle](/gocdnext/docs/concepts/services/) — sibling service containers (postgres, redis, …) for integration tests.
- [Materials](/gocdnext/docs/concepts/materials/) — implicit project material + `when.event:` triggers.
