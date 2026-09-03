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
POST <PUBLIC_BASE>/api/webhooks/<provider>
```

Where `<provider>` is `github`, `gitlab`, or `bitbucket`. The
endpoint is unauthenticated by URL but verified by HMAC against
a per-source secret — same shape every webhook integration uses.

`<PUBLIC_BASE>` is `GOCDNEXT_PUBLIC_BASE` (or the override in
`GOCDNEXT_WEBHOOK_PUBLIC_URL` when behind a public tunnel +
private dashboard). The chart's `webhookPublicURL` value sets
the latter.

### Fan-out to multiple pipelines

A single push fans out to every pipeline whose materials match
the push's `(repo, branch)` fingerprint. So a monorepo with
`ci-server.yaml`, `ci-web.yaml`, and `security.yaml` all sharing
the implicit project material gets **three** runs from one push,
all dispatched concurrently. The webhook response body is:

```
HTTP/1.1 202 Accepted
{ "runs": ["<uuid>", "<uuid>", "<uuid>"] }
```

(Pre-v0.4.20 it was a single run per push; if you have downstream
tooling that expected one id, switch it to iterate the `runs`
array.)

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

### Required App permissions

Set these **repository permissions** on the GitHub App (its *Settings →
Permissions*). Adding one later requires each org's admin to **re-approve** the
App.

| Permission | Access | Why |
|---|---|---|
| Metadata | Read | mandatory baseline |
| Contents | Read | clone the repo for a run |
| Pull requests | Read | PR head SHA + PR events |
| Webhooks | Read & write | auto-register the webhook (above) |
| **Checks** | **Read & write** | post the `gocdnext / <pipeline>` **check run** — the rich, GitHub-hosted view (coverage + security summary, re-run button) |
| **Commit statuses** | **Read & write** | post the `ci/gocdnext/<project>/<pipeline>` **commit status** — the check row whose link goes **straight to the run** (the UX Woodpecker/GoCD give). A SEPARATE permission from Checks. |
| **Merge queues** | **Read** | receive GitHub App `merge_group` webhooks so required PR checks also run on merge-queue SHAs |
| **Administration** | **Read & write** | *(optional)* write the repo's **required-checks ruleset** so gocdnext can manage "required pipelines for PR merge" from its UI. Only needed if you use that feature — omit it and everything else still works. |

gocdnext posts **both** per pipeline: the **check run** (rich view; its run link
is the "Details" button) and the **commit status** (plain row, links straight to
the run). If you grant only **Checks: write**, you still get the check run — the
commit status **degrades gracefully** (a WARN in the server log on the `403`, the
run never fails). Grant **Commit statuses: write** to get the straight-to-run
entry too.

Set the GitHub App's own **Webhook URL** to
`https://ci.example.com/api/webhooks/github/app` and keep the App webhook secret
in *Settings → SCM integrations*. That App webhook receives `check_run` /
`check_suite` re-run requests and GitHub merge queue `merge_group` events. The
per-repo webhook below still handles push / pull request / tag deliveries at
`/api/webhooks/github`.

For merge queue support, subscribe the GitHub App to **Merge group** events.
`checks_requested` queues the PR-required pipelines against GitHub's queue SHA;
`destroyed` cancels active runs for an abandoned queue SHA without reporting a
red/canceled status back to GitHub.

The commit-status context is `ci/gocdnext/<project>/<pipeline>`, project-qualified
so two projects on the same repo don't overwrite each other's status. GitHub
compares contexts **case-insensitively**, so keep project/pipeline names
repo-unique even across case — two that differ only by case would still collide.

:::note[Making a check *required*]
gocdnext posts these checks; whether GitHub **requires** one for merge is a
**branch ruleset / branch-protection** setting on the repo or org (require the
context `ci/gocdnext/<project>/<pipeline>` or the check `gocdnext / <pipeline>`). An
**org-level ruleset** requires it across many repos by name — no per-repo config.

gocdnext can **configure that ruleset for you** — *Project → Settings → Required
checks* picks which pipelines must be green to merge, and gocdnext writes a
dedicated per-project `gocdnext-required-checks-<project>` ruleset on the repo.
That needs the
**Administration: write** permission above. See
[Required checks for merge](/pipelines/required-checks/).
:::

### Manual setup

Per-repo: *Settings → Webhooks → Add webhook*.

| Field | Value |
|---|---|
| Payload URL | `https://ci.example.com/api/webhooks/github` |
| Content type | `application/json` |
| Secret | (any random string — copy to gocdnext below) |
| SSL verification | Enable |
| Events | Push events, Pull requests, Tags created |
| Active | ✓ |

Do not send GitHub merge queue events to this per-repo endpoint; `merge_group`
is handled by the GitHub App webhook at `/api/webhooks/github/app`.

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
| URL | `https://ci.example.com/api/webhooks/gitlab` |
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
| URL | `https://ci.example.com/api/webhooks/bitbucket` |
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

There is no generic webhook endpoint today. For Gitea / Forgejo
and other self-hosted Git hosting, point the provider's webhook
at the most-compatible of the three real endpoints (Gitea ↔
`/api/webhooks/github` is the closest payload match) and accept
that exotic providers may need a shim.

The pre-v0.4 `GOCDNEXT_WEBHOOK_TOKEN` env var that gated a
generic endpoint is deprecated — the server warns on boot if it's
set and ignores the value. Per-source secrets via the SCM-source
admin flow replaced it.

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

## When a push doesn't create a run

A push that doesn't match any pipeline's `when:` filter (event +
branch mismatch) results in **no run created** — the dashboard
shows nothing, but the webhook delivery log shows 200 OK (server
received and processed correctly, just nothing matched). To debug:

- Set `GOCDNEXT_LOG_LEVEL=debug` temporarily — the server logs
  every filter decision.
- Check the project's pipelines: do their pipeline-level `when:`
  blocks list the branch the push hit? Remember `branch:` is
  singular (the parser rejects `branches:`).
- Path-based filtering isn't supported in `when:` — if you split
  pipelines per sub-tree to scope runs, that's the right model;
  if you put `paths:` under `when:` expecting it to be honored,
  it isn't.

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
