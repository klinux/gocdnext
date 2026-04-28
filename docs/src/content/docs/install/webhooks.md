---
title: Webhook setup per provider
description: Step-by-step for wiring GitHub, GitLab, and Bitbucket webhooks so pushes turn into runs.
---

gocdnext is webhook-first: a push to a connected repo creates a
run, no polling needed. This page walks the setup for each of the
three supported providers. The platform's *Settings → SCM
integrations* + the per-project *Connect repo* flows automate
most of this — manual setup is here as a fallback when
auto-register doesn't apply (self-hosted, restricted org policies,
existing webhooks you want to reuse).

## What gocdnext expects

Whatever provider, the webhook delivers events to:

```
POST <PUBLIC_BASE>/api/v1/webhook/<provider>
```

Where `<provider>` is `github`, `gitlab`, or `bitbucket`. The
endpoint is unauthenticated by URL but verified by HMAC against
a per-source secret — same shape every webhook integration uses.

`<PUBLIC_BASE>` is `GOCDNEXT_PUBLIC_BASE` (or the override in
`GOCDNEXT_WEBHOOK_PUBLIC_URL` when behind a public tunnel +
private dashboard). The chart's `webhookPublicURL` value sets
the latter.

## GitHub

### Auto-register (recommended)

If you've connected gocdnext as a **GitHub App** with `Repository:
Webhooks` permission, the apply flow auto-registers the webhook on
new repos:

1. *Settings → SCM integrations → Add → GitHub App*.
2. Install the app on your org / specific repos.
3. Apply a project pointing at one of those repos — gocdnext sees
   the install, registers the webhook, you're done.

For OAuth-only flows (no App), the auto-register works if the
authenticated user has admin on the repo. The platform creates
the webhook via the REST API.

### Manual setup

Per-repo: *Settings → Webhooks → Add webhook*.

| Field | Value |
|---|---|
| Payload URL | `https://ci.example.com/api/v1/webhook/github` |
| Content type | `application/json` |
| Secret | (any random string — copy to gocdnext below) |
| SSL verification | Enable |
| Events | Push events, Pull requests, Tags created |
| Active | ✓ |

After the webhook is created, register the secret with gocdnext:

1. *Project → Settings → SCM source*.
2. Paste the secret you used in the GitHub form into
   *Webhook secret*.
3. Save.

### GitHub Enterprise

Same flow, just the `Payload URL` points at your gocdnext
instance and the SCM source `apiBase:` is set to your GHE API
URL (`https://github.example.com/api/v3`). Auth via GHE works
identically — see [Auth deep-dive](/gocdnext/docs/install/auth/).

## GitLab

### Auto-register (recommended)

*Settings → SCM integrations → Add → GitLab*.

You need a GitLab access token with `api` scope (Personal Access
Token or Project Access Token). Paste it; gocdnext stores it
encrypted, uses it to register webhooks on apply.

For self-hosted GitLab: set `apiBase:` to your instance URL
(`https://gitlab.example.com/api/v4`). The same token format
works.

### Manual setup

Per-project: *Settings → Webhooks*.

| Field | Value |
|---|---|
| URL | `https://ci.example.com/api/v1/webhook/gitlab` |
| Secret token | (any random string) |
| Events | Push events, Tag push events, Merge request events |
| SSL verification | Enable |

Register the secret with gocdnext: *Project → Settings → SCM
source → Webhook secret*.

### GitLab system-level webhook (instance-wide)

Self-hosted instances can register a single webhook for the entire
instance — gocdnext receives every push to every project. Useful
when you don't want to manage per-project webhooks but downside
is the platform's gocdnext server now sees push traffic from
projects it doesn't manage (silently dropped, but adds noise).

The trade-off rarely pays off; per-project webhooks are the
sensible default.

## Bitbucket

### Auto-register

Same shape as GitLab: store a Bitbucket app password (with
`webhook:write`) in *SCM integrations*; gocdnext registers
webhooks on apply.

### Manual setup

Per-repo: *Repository settings → Webhooks → Add webhook*.

| Field | Value |
|---|---|
| Title | gocdnext |
| URL | `https://ci.example.com/api/v1/webhook/bitbucket` |
| Active | ✓ |
| Triggers | Repository push, Pull request created/updated, Tag created |

Bitbucket Cloud doesn't have a per-webhook secret in the UI; the
platform falls back to verifying the request's source IP against
Bitbucket's documented IP ranges. For tighter security, use
Bitbucket Data Center which supports webhook secrets.

For Bitbucket Cloud, the `Webhook secret` field in gocdnext is
still useful — set it to a random string and pass it in the URL
query (`?secret=...`) on the webhook URL when you create it.
The platform validates it.

## Generic webhook (self-hosted Git, custom integrations)

For Gitea, Forgejo, custom git hosting, or anything else that can
do an HTTP POST on push:

```
POST <PUBLIC_BASE>/api/v1/webhook/generic?token=<token>
Content-Type: application/json

{
  "repo": "https://git.example.com/myorg/myapp",
  "branch": "main",
  "sha": "abc123def...",
  "event": "push"
}
```

`token` is `GOCDNEXT_WEBHOOK_TOKEN` (Helm: `webhookToken.value`).
The body shape is gocdnext's normalised event format — your
custom hook's outgoing payload should be transformed to this
shape (a small middleware function, ~20 lines in any language).

## Verifying delivery

After the webhook is wired:

1. Push a trivial commit to the connected repo.
2. The provider's webhook delivery log should show 200 OK.
3. *Project → Recent runs* in the dashboard shows a new run within
   ~1 second of the push.

If the run doesn't appear:

- Check the gocdnext server log: `kubectl -n gocdnext logs
  deployment/gocdnext-server --tail=100 | grep webhook`. HMAC
  failures, missing-source errors, or YAML parse errors all
  surface here.
- Verify the webhook secret matches between the provider and
  gocdnext. Mismatched secrets show up as `webhook: HMAC
  validation failed`.
- Verify the `<PUBLIC_BASE>` is reachable from the provider's
  delivery IPs. Behind a corporate firewall? Open the relevant
  IP ranges or use a tunnel (smee.io, ngrok) for dev.

## When `paths:` filters skip a run

A push that matches `when.paths:` filter results in **no run
created**. The dashboard shows nothing, the webhook delivery
log shows 200 OK (server received + processed correctly, just
filtered out). To debug:

- Set `GOCDNEXT_LOG_LEVEL=debug` temporarily — the server logs
  every filter decision.
- Check the actual changed-files list in the webhook payload
  against your `paths:` glob. The provider's *Recent
  deliveries* shows the request body.

## Auto-register caveats

- **GitHub App permissions can drift**. If an admin removes the
  app's webhook permission later, future apply calls will silently
  fail to register. Watch the project apply log for
  `auto_register_webhook: failed`.
- **Org-level rate limits**. GitHub limits webhook creations per
  hour. Mass-applying 100 projects at once can hit it; spread
  applies or pre-create webhooks manually.
- **Self-hosted GitLab + IP allowlists**. The webhook from
  GitLab's runner IP needs to reach gocdnext's `<PUBLIC_BASE>`.
  In closed networks, configure both ends to be on the same
  internal subnet or whitelist explicitly.
