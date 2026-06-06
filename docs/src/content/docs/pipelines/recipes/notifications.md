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
      webhook: ${{ SLACK_WEBHOOK }}
      channel: "#ci-alerts"
      template: |
        :rotating_light: *${CI_PIPELINE}* #${CI_RUN_COUNTER} failed
        commit `${CI_COMMIT_SHA}` on `${CI_COMMIT_BRANCH}`
    secrets: [SLACK_WEBHOOK]
```

`on:` accepts `success`, `failure`, `canceled`, `always` — single
'l' on `canceled` (the parser canonical form). The notification
fires only when the run's terminal status matches.

Substitution rules apply: `${{ NAME }}` is identifier-only (no
dotted `${{ secrets.X }}` — list the name in `secrets:` and refer
to it as `${{ NAME }}`). `${VAR}` is shell-style and reaches the
plugin verbatim for runtime expansion.

## Plugins available

### Slack

Slack ships **one** body field: `template:`. There's no separate
title/message split — fold both into one Slack-mrkdwn block.

```yaml
- on: failure
  uses: gocdnext/slack@v1
  with:
    webhook: ${{ SLACK_WEBHOOK }}    # Incoming webhook URL
    channel: "#ci-alerts"            # overrides webhook default
    template: |
      :x: *${CI_PIPELINE}* #${CI_RUN_COUNTER} failed
      commit `${CI_COMMIT_SHA}`
  secrets: [SLACK_WEBHOOK]
```

Slack incoming webhooks at *Apps → Incoming Webhooks → Add to
Workspace*. Webhook URL goes in project secrets. Empty
`template:` falls back to a default "pipeline #N → status (sha)"
line built from CI_* vars.

### Discord

Discord uses `content:` (not `template:`). Markdown rendering
matches Discord's flavour (bold, code, mentions).

```yaml
- on: failure
  uses: gocdnext/discord@v1
  with:
    webhook: ${{ DISCORD_WEBHOOK }}
    content: |
      **${CI_PIPELINE}** #${CI_RUN_COUNTER} failed
      commit `${CI_COMMIT_SHA}`
    username: "gocdnext"             # optional bot name override
  secrets: [DISCORD_WEBHOOK]
```

Discord webhook URL from *Server Settings → Integrations →
Webhooks → New Webhook*.

### Microsoft Teams

Teams accepts `title:` + `message:` (and an optional
`theme-color:` hex without the leading `#`).

```yaml
- on: failure
  uses: gocdnext/teams@v1
  with:
    webhook: ${{ TEAMS_WEBHOOK }}
    title: "${CI_PIPELINE} failed"
    message: |
      Run #${CI_RUN_COUNTER}, commit ${CI_COMMIT_SHA}.
    theme-color: "d13438"            # red for failure
  secrets: [TEAMS_WEBHOOK]
```

Teams "Incoming Webhook" connector. Same flow as Slack/Discord —
URL into project secrets.

### Email (SMTP)

Email has the largest required-fields surface. `host:`, `from:`,
`to:`, `subject:`, `body:` are all mandatory — SMTP is configured
with explicit headers, no inference.

```yaml
- on: failure
  uses: gocdnext/email@v1
  with:
    host: smtp.sendgrid.net
    port: "587"
    tls: starttls
    username: ${{ SMTP_USER }}
    password: ${{ SMTP_PASSWORD }}
    from: "gocdnext CI <ci@mycorp.com>"
    to: "oncall@mycorp.com"
    subject: "[CI] ${CI_PIPELINE} #${CI_RUN_COUNTER} failed"
    body: |
      Pipeline ${CI_PIPELINE} failed.
      Commit: ${CI_COMMIT_SHA}
  secrets: [SMTP_USER, SMTP_PASSWORD]
```

`tls:` is `starttls` (port 587, default), `tls` (port 465), or
`none` (port 25 for unauthenticated relay inside a corp net).

### Matrix

Matrix is a real chat protocol — input names reflect the API.
The room is identified by `room-id:` (id `!abc:server` or alias
`#eng:server`); the message is `body:` (plain) plus optional
`html:` for rich formatting.

```yaml
- on: failure
  uses: gocdnext/matrix@v1
  with:
    homeserver: https://chat.mycorp.com
    token: ${{ MATRIX_TOKEN }}
    room-id: "#eng:chat.mycorp.com"
    msgtype: m.notice                # m.text | m.notice (default m.text)
    body: |
      [PROD] ${CI_PIPELINE} #${CI_RUN_COUNTER} failed
      commit ${CI_COMMIT_SHA} on ${CI_COMMIT_BRANCH}
  secrets: [MATRIX_TOKEN]
```

Matrix tokens via the `/_matrix/client/r0/login` flow — store the
result in project secrets.

## Template variables

Every notifier expands the agent-injected CI variables in body
fields at runtime:

| Variable | Example |
|---|---|
| `${CI_PIPELINE}` | `ci-server` |
| `${CI_PIPELINE_STATUS}` | `failed` / `success` / `canceled` |
| `${CI_RUN_COUNTER}` | `42` |
| `${CI_RUN_ID}` | UUID |
| `${CI_COMMIT_SHA}` | full revision SHA |
| `${CI_COMMIT_SHORT_SHA}` | 8-char prefix |
| `${CI_COMMIT_BRANCH}` | branch name |
| `${CI_JOB_NAME}` | job name (notification-job synthetic name) |

See [YAML reference → CI built-ins](/gocdnext/docs/pipelines/yaml-reference/#ci-built-ins)
for the complete list the platform injects.

## Common patterns

### Loud on failure, quiet on success

```yaml
notifications:
  - on: failure
    uses: gocdnext/slack@v1
    with:
      webhook: ${{ SLACK_WEBHOOK }}
      channel: "#ci-alerts"
    secrets: [SLACK_WEBHOOK]
```

The default — alert when something breaks, stay quiet otherwise.
Saves the team's attention budget.

### Different channels per branch

The platform doesn't natively branch-template the channel, but
you can use a per-pipeline-file split:

```yaml title=".gocdnext/ci-main.yaml"
when:
  event: [push]
  branch: [main]
notifications:
  - on: success
    uses: gocdnext/slack@v1
    with:
      webhook: ${{ SLACK_WEBHOOK }}
      channel: "#deploys"
    secrets: [SLACK_WEBHOOK]
  - on: failure
    uses: gocdnext/slack@v1
    with:
      webhook: ${{ SLACK_WEBHOOK }}
      channel: "#alerts-prod"
    secrets: [SLACK_WEBHOOK]
```

```yaml title=".gocdnext/ci-feature.yaml"
when:
  event: [push]
notifications:
  - on: failure
    uses: gocdnext/slack@v1
    with:
      webhook: ${{ SLACK_WEBHOOK }}
      channel: "#alerts-dev"
    secrets: [SLACK_WEBHOOK]
```

Two pipeline files, same shape; the `when:` filter routes the
right one to the right channel.

### Multiple sinks for one event

You can list as many notifications as you want; they all fire in
parallel. A typical "production" project ends up with:

```yaml
notifications:
  - on: failure
    uses: gocdnext/slack@v1
    with:
      webhook: ${{ SLACK_WEBHOOK }}
      channel: "#ci-alerts"
    secrets: [SLACK_WEBHOOK]
  - on: failure
    uses: gocdnext/teams@v1
    with:
      webhook: ${{ TEAMS_WEBHOOK }}
      title: "${CI_PIPELINE} failed"
      message: "Run #${CI_RUN_COUNTER}, commit ${CI_COMMIT_SHA}."
    secrets: [TEAMS_WEBHOOK]
  - on: failure
    uses: gocdnext/email@v1
    with:
      host: smtp.sendgrid.net
      port: "587"
      tls: starttls
      username: ${{ SMTP_USER }}
      password: ${{ SMTP_PASSWORD }}
      from: "gocdnext CI <ci@mycorp.com>"
      to: "oncall@mycorp.com"
      subject: "[CI] ${CI_PIPELINE} failed"
      body: "Run ${CI_RUN_COUNTER} failed on ${CI_COMMIT_SHA}."
    secrets: [SMTP_USER, SMTP_PASSWORD]
```

## Common pitfalls

- **Webhook URL inline in YAML**: never. Always declare it as a
  project secret and reference via `${{ NAME }}` with `secrets:
  [NAME]` — webhooks ARE credentials.
- **Dotted references**: `${{ secrets.X }}` is rejected with
  "unsupported reference expression". The parser supports
  identifier-only refs (`${{ X }}`) — list the name under
  `secrets:` and use it directly.
- **Notification storms**: a flapping pipeline fires on every
  failure. Pair with retention policies and consider a dedup
  downstream tool (PagerDuty deduplicates by title).
- **Discord rate limits**: 30 webhook posts per minute per
  channel. A noisy CI fleet that all dumps into `#deploys` will
  hit it.
- **Email reaching production inboxes**: SMTP credentials should
  point at a transactional service (Postmark, SendGrid,
  internal relay) — not your personal Gmail. Spam filters bin
  notifications-from-CI as bulk.
- **No `awaiting_approval` trigger**: the platform doesn't fire a
  notification when a job enters `awaiting_approval`. If you need
  this, watch the issue tracker for the feature, or hook
  Postgres notifications externally.
