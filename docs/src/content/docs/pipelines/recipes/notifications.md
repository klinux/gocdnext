---
title: Notifications fan-out
description: Tell humans when a build needs them — Slack, Discord, Teams, email, Matrix.
---

The platform has five notification plugins, all wired the same
way. They run **after** the run terminates as synthetic jobs in
the run's audit trail — operators see exactly which notification
fired and what payload was sent.

## Where notifications live in YAML

Top-level `notifications:` array, separate from `jobs:`:

```yaml
name: ci
when:
  event: [push, pull_request]
stages: [test, build]
jobs:
  ...

notifications:
  - on: failure
    uses: gocdnext/slack@v1
    with:
      webhook: ${{ secrets.SLACK_WEBHOOK }}
      channel: "#ci-alerts"
      title: "🚨 ${CI_PROJECT_SLUG} — ${CI_PIPELINE_NAME} failed"
      message: |
        *Branch*: `${CI_COMMIT_BRANCH}`
        *Author*: ${CI_COMMIT_AUTHOR}
        *Commit*: ${CI_COMMIT_SHORT_SHA}
        *Run*: ${CI_RUN_URL}
```

`on:` accepts `success`, `failure`, `cancelled`, `always`. The
notification fires only when the run's terminal status matches.

## Plugins available

### Slack

```yaml
- on: failure
  uses: gocdnext/slack@v1
  with:
    webhook: ${{ secrets.SLACK_WEBHOOK }}    # Incoming webhook URL
    channel: "#ci-alerts"                    # override default channel
    title: "..."
    message: "..."
```

Slack incoming webhooks at *Apps → Incoming Webhooks → Add to
Workspace*. Webhook URL goes in project secrets.

### Discord

```yaml
- on: failure
  uses: gocdnext/discord@v1
  with:
    webhook: ${{ secrets.DISCORD_WEBHOOK }}
    title: "..."
    message: "..."
```

Discord webhook URL from *Server Settings → Integrations →
Webhooks → New Webhook*.

### Microsoft Teams

```yaml
- on: failure
  uses: gocdnext/teams@v1
  with:
    webhook: ${{ secrets.TEAMS_WEBHOOK }}
    title: "..."
    message: "..."
```

Teams "Incoming Webhook" connector. Same flow as Slack/Discord —
URL into project secrets.

### Email (SMTP)

```yaml
- on: failure
  uses: gocdnext/email@v1
  with:
    to: oncall@example.com
    subject: "[gocdnext] ${CI_PROJECT_SLUG} failed"
    body: |
      Run: ${CI_RUN_URL}
      Branch: ${CI_COMMIT_BRANCH}
  secrets:
    - SMTP_HOST
    - SMTP_PORT
    - SMTP_USERNAME
    - SMTP_PASSWORD
```

### Matrix

```yaml
- on: failure
  uses: gocdnext/matrix@v1
  with:
    homeserver: https://matrix.example.com
    room: "!abc123:example.com"
    title: "..."
    message: "..."
  secrets:
    - MATRIX_TOKEN
```

Matrix tokens from `/_matrix/client/r0/login` flow — store the
result in project secrets.

## Template variables

Every notification plugin gets these variables substituted at
dispatch time:

| Variable | Example |
|---|---|
| `${CI_PROJECT_SLUG}` | `myapp` |
| `${CI_PIPELINE_NAME}` | `ci-server` |
| `${CI_COMMIT_BRANCH}` | `main` |
| `${CI_COMMIT_SHORT_SHA}` | `abc1234` |
| `${CI_COMMIT_AUTHOR}` | `Alice <alice@example.com>` |
| `${CI_RUN_ID}` | UUID |
| `${CI_RUN_COUNTER}` | `42` |
| `${CI_RUN_STATUS}` | `success` / `failed` / `cancelled` |
| `${CI_RUN_URL}` | full URL to the run detail page |
| `${CI_DURATION_SEC}` | wall-clock seconds |

## Common patterns

### Loud on failure, quiet on success

```yaml
notifications:
  - on: failure
    uses: gocdnext/slack@v1
    with: { webhook: ..., channel: "#ci-alerts" }
```

The default — alert when something breaks, stay quiet otherwise.
Saves the team's attention budget.

### Different channels per branch

The platform doesn't natively branch-template the channel, but
you can use a per-pipeline-file split:

```yaml title=".gocdnext/ci.yaml"
when:
  branches: [main]
notifications:
  - on: success
    uses: gocdnext/slack@v1
    with: { webhook: ..., channel: "#deploys" }
  - on: failure
    uses: gocdnext/slack@v1
    with: { webhook: ..., channel: "#alerts-prod" }
```

```yaml title=".gocdnext/ci-feature.yaml"
when:
  branches: ["feature/**"]
notifications:
  - on: failure
    uses: gocdnext/slack@v1
    with: { webhook: ..., channel: "#alerts-dev" }
```

Two pipeline files, same shape; the `when:` filter routes the
right one to the right channel.

### Approval-pending notification

When a pipeline has an approval gate, notify the approver group
that something's waiting:

```yaml
notifications:
  - on: awaiting_approval     # the fifth implicit `on:` value
    uses: gocdnext/slack@v1
    with:
      webhook: ${{ secrets.PROD_SLACK_WEBHOOK }}
      channel: "#prod-deploys"
      title: "🟡 Approval needed"
      message: |
        *${CI_PROJECT_SLUG}* is at the prod gate.
        Approve at: ${CI_RUN_URL}
```

`awaiting_approval` fires the moment a job hits that status, not
when the run terminates. Use this OR a follow-up notification —
not both, or you'll spam.

### Multiple sinks for one event

You can list as many notifications as you want; they all fire in
parallel. A typical "production" project ends up with:

```yaml
notifications:
  - on: failure
    uses: gocdnext/slack@v1
    with: { webhook: ..., channel: "#ci-alerts" }
  - on: failure
    uses: gocdnext/teams@v1
    with: { webhook: ..., title: "${CI_PROJECT_SLUG} failed" }
  - on: failure
    uses: gocdnext/email@v1
    with: { to: oncall@example.com, ... }
```

## Common pitfalls

- **Webhook URL in YAML**: never inline. Always via `secrets:`
  with `${{ secrets.NAME }}` — webhooks are credentials.
- **Notification storms**: a flapping pipeline notifies on every
  failure. Pair with the `keepLast` retention policy (Helm
  `caches.keepLast`) and consider a dedup downstream tool
  (PagerDuty deduplicates by title).
- **Discord rate limits**: 30 webhook posts per minute per
  channel. A noisy CI fleet that all dumps into `#deploys` will
  hit it. Spread across multiple channels or use a buffer
  service.
- **Email reaching production inboxes**: SMTP credentials should
  point at a transactional service (Postmark, SendGrid,
  internal relay) — not your personal Gmail. Spam filters bin
  notifications-from-CI as bulk.
